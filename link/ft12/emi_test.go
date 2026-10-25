package ft12

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/shabunin/tiny-knx-go/cemi"
)

// respondEMI answers like an EMI2 module; cEMI requests are ignored.
func (m *fakeModule) respondEMI(payload []byte) {
	m.mu.Lock()
	addr := m.address
	m.mu.Unlock()

	switch payload[0] {
	case emiPEISwitchReq:
		m.mu.Lock()
		m.emiMode = payload
		m.mu.Unlock()

	case emiPEIIdentifyReq:
		m.sendPayload([]byte{emiPEIIdentifyCon, byte(addr >> 8), byte(addr), 0x00, 0xc5, 0x01, 0x00, 0x41, 0x22})

	case emiPCSetValueReq:
		if bytes.Equal(payload[:4], []byte{emiPCSetValueReq, 2, 0x01, 0x17}) {
			m.mu.Lock()
			m.address = cemi.IndividualAddr(payload[4])<<8 | cemi.IndividualAddr(payload[5])
			m.writes++
			m.mu.Unlock()
		}

	case byte(cemi.LDataReqCode):
		// The confirmation carries the source; the error bit marks a frame nobody acknowledged.
		con := append([]byte{byte(cemi.LDataConCode), payload[1] | 0xB0, byte(addr >> 8), byte(addr)}, payload[4:]...)
		if cemi.IndividualAddr(payload[4])<<8|cemi.IndividualAddr(payload[5]) == unreachableAddr {
			con[1] |= 1
		}

		m.sendPayload(con)
	}
}

func openEMITestLink(t *testing.T, protocol Protocol) (*Link, *fakeModule) {
	t.Helper()

	host, modulePort := serialLine()
	module := newFakeModule(modulePort)
	module.emi2 = true

	config := testConfig
	config.Protocol = protocol

	l, err := Open(host, config)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	t.Cleanup(func() {
		l.Close()
		modulePort.Close()
	})

	return l, module
}

func TestEMIConversion(t *testing.T) {
	// Taken from an old Weinzierl BAOS module at 1.1.4 and a kBerry at 1.1.42.
	tests := []struct {
		name      string
		cemi, emi string
	}{
		{"group read", "11 00 bc e0 00 00 ff ff 01 00 00", "11 0c 00 00 ff ff e1 00 00"},
		{"group write", "11 00 bc e0 00 00 0a 03 02 00 80 2a", "11 0c 00 00 0a 03 e2 00 80 2a"},
		{"connect", "11 00 b0 60 00 00 11 04 00 80", "11 00 00 00 11 04 60 80"},
	}

	for _, test := range tests {
		emi, err := emiFromCEMI(unhex(t, test.cemi))
		if err != nil || !bytes.Equal(emi, unhex(t, test.emi)) {
			t.Errorf("%s: emiFromCEMI = % x, %v; want %s", test.name, emi, err, test.emi)
		}
	}

	frames := []struct {
		name      string
		emi, cemi string
	}{
		{"con", "2e bc 11 04 ff ff e1 00 00", "2e 00 bc e0 11 04 ff ff 01 00 00"},
		{"ind", "29 bc 11 2a ff fe e1 00 00", "29 00 bc e0 11 2a ff fe 01 00 00"},
		{"ind individual", "29 b0 11 2a 11 04 60 80", "29 00 b0 60 11 2a 11 04 00 80"},
	}

	for _, f := range frames {
		frame, ok := emiToCEMI(unhex(t, f.emi))
		if !ok || !bytes.Equal(frame, unhex(t, f.cemi)) {
			t.Errorf("%s: emiToCEMI = % x, %v; want %s", f.name, frame, ok, f.cemi)
		}

		var msg cemi.Message
		if _, err := cemi.Unpack(frame, &msg); err != nil {
			t.Errorf("%s: cemi.Unpack: %v", f.name, err)
		}
	}

	if _, ok := emiToCEMI([]byte{emiPEIIdentifyCon, 0x11, 0x04}); ok {
		t.Error("emiToCEMI converted PEI_Identify.con")
	}

	long := groupWrite(0x0a03, 0)
	long.Data = &cemi.AppData{Command: cemi.GroupValueWrite, Data: make([]byte, 16)}

	if _, err := emiFromCEMI(packed(long)); err != ErrFrameTooLong {
		t.Errorf("emiFromCEMI of an extended frame: %v, want %v", err, ErrFrameTooLong)
	}
}

func TestEMIDetect(t *testing.T) {
	for _, protocol := range []Protocol{ProtocolAuto, ProtocolEMI2} {
		l, module := openEMITestLink(t, protocol)

		if l.Protocol() != ProtocolEMI2 {
			t.Errorf("%v: Protocol %v", protocol, l.Protocol())
		}

		module.waitFor(t, "link layer mode", func() bool { return bytes.Equal(module.emiMode, emiSwitchLinkLayer) })

		if addr, err := l.ReadAddress(); err != nil || addr != moduleAddr || l.Address() != moduleAddr {
			t.Errorf("%v: ReadAddress = %v, %v; Address %v", protocol, addr, err, l.Address())
		}

		if _, err := l.ReadProperty(cemi.ObjectTypeDevice, 1, cemi.PIDDeviceDescriptor, 1, 1); err != ErrNotSupported {
			t.Errorf("%v: ReadProperty: %v", protocol, err)
		}

		l.Close()
		module.waitFor(t, "normal mode", func() bool { return bytes.Equal(module.emiMode, emiSwitchNormal) })
	}
}

func TestEMISendReceive(t *testing.T) {
	l, module := openEMITestLink(t, ProtocolAuto)

	if err := l.Send(groupWrite(0x0a03, 1)); err != nil {
		t.Fatalf("Send: %v", err)
	}

	frames := module.dataFrames()
	if got, want := frames[len(frames)-1].Payload, unhex(t, "11 0c 00 00 0a 03 e1 00 81"); !bytes.Equal(got, want) {
		t.Errorf("Sent % x, want % x", got, want)
	}

	unreachable := groupWrite(uint16(unreachableAddr), 1)
	unreachable.Control2 = cemi.Control2Hops(6)

	if err := l.Send(unreachable); err == nil {
		t.Error("Send to an unreachable device succeeded")
	}

	module.sendPayload(unhex(t, "29 bc 11 2a 0a 03 e1 00 80"))

	ind := <-l.Inbound()
	if ind.Source != cemi.NewIndividualAddr3(1, 1, 42) || ind.Destination != 0x0a03 || !ind.Control2.IsGroupAddr() {
		t.Errorf("Received %+v", ind.LData)
	}

	app, ok := ind.Data.(*cemi.AppData)
	if !ok || app.Command != cemi.GroupValueWrite || !bytes.Equal(app.Data, []byte{0}) {
		t.Errorf("Received data %+v", ind.Data)
	}
}

func TestEMISetAddress(t *testing.T) {
	l, module := openEMITestLink(t, ProtocolEMI2)

	newAddr := cemi.NewIndividualAddr3(1, 1, 77)
	if err := l.SetAddress(newAddr); err != nil {
		t.Fatalf("SetAddress: %v", err)
	}

	if err := l.SetAddress(newAddr); err != nil {
		t.Fatalf("SetAddress again: %v", err)
	}

	module.mu.Lock()
	defer module.mu.Unlock()

	if module.address != newAddr || module.writes != 1 || l.Address() != newAddr {
		t.Errorf("Module address %v after %d writes, link address %v", module.address, module.writes, l.Address())
	}
}

func TestEMIModuleResetRestoresLinkLayer(t *testing.T) {
	_, module := openEMITestLink(t, ProtocolAuto)

	module.waitFor(t, "link layer mode", func() bool { return bytes.Equal(module.emiMode, emiSwitchLinkLayer) })

	module.mu.Lock()
	module.emiMode = nil
	module.fcb = true
	module.mu.Unlock()

	module.port.Write(AppendFixed(nil, ControlResetInd))

	module.waitFor(t, "link layer mode again", func() bool { return bytes.Equal(module.emiMode, emiSwitchLinkLayer) })
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()

	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatal(err)
	}

	return b
}
