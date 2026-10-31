package usb

import (
	"bytes"
	"io"
	"sync"
	"testing"

	"github.com/shabunin/tiny-knx-go/cemi"
	"github.com/shabunin/tiny-knx-go/link/ft12"
)

func TestReports(t *testing.T) {
	for _, size := range []int{0, 1, 53, 54, 114, 115, maxBody} {
		body := make([]byte, size)
		for i := range body {
			body[i] = byte(i)
		}

		reports, err := transfer{protocol: protocolTunnel, service: byte(CEMI), body: body}.reports()
		if err != nil {
			t.Fatal(err)
		}

		var a assembler

		for i, r := range reports {
			if len(r) != ReportSize || r[0] != reportID || r[1]>>4 != byte(i+1) {
				t.Fatalf("size %d: report %d: % x", size, i, r[:3])
			}

			got, ok := a.add(r)
			if ok != (i == len(reports)-1) {
				t.Fatalf("size %d: report %d complete %v", size, i, ok)
			}

			if ok && (got.protocol != protocolTunnel || got.service != byte(CEMI) || !bytes.Equal(got.body, body)) {
				t.Fatalf("size %d: got %+v", size, got)
			}
		}
	}

	if _, err := (transfer{body: make([]byte, maxBody+1)}).reports(); err != ErrTooLong {
		t.Fatalf("too long: %v", err)
	}
}

func TestSingleReport(t *testing.T) {
	reports, _ := transfer{protocol: protocolBusAccess, service: serviceFeatureGet, body: []byte{byte(FeatureSupportedEMI)}}.reports()

	want := []byte{0x01, 0x13, 0x09, 0x00, 0x08, 0x00, 0x01, 0x0F, 0x01, 0x00, 0x00, 0x01}
	if len(reports) != 1 || !bytes.Equal(reports[0][:len(want)], want) {
		t.Fatalf("got % x", reports[0][:len(want)])
	}
}

// fakeInterface is a cEMI USB interface that confirms every frame.
type fakeInterface struct {
	t         *testing.T
	supported uint16
	reports   chan []byte
	closed    chan struct{}
	once      sync.Once
	a         assembler

	mu        sync.Mutex
	activeEMI byte
	commMode  byte
	sent      []cemi.Message
}

func newFakeInterface(t *testing.T, supported uint16) *fakeInterface {
	return &fakeInterface{t: t, supported: supported, reports: make(chan []byte, 16), closed: make(chan struct{}), commMode: cemi.CommModeDataLinkLayer}
}

func (f *fakeInterface) Read(p []byte) (int, error) {
	select {
	case r := <-f.reports:
		return copy(p, r), nil
	case <-f.closed:
		return 0, io.EOF
	}
}

func (f *fakeInterface) Close() error {
	f.once.Do(func() { close(f.closed) })
	return nil
}

func (f *fakeInterface) answer(t transfer) {
	reports, err := t.reports()
	if err != nil {
		f.t.Error(err)
	}

	for _, r := range reports {
		f.reports <- r
	}
}

func (f *fakeInterface) Write(p []byte) (int, error) {
	t, ok := f.a.add(p)
	if !ok {
		return len(p), nil
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case t.protocol == protocolBusAccess && t.service == serviceFeatureGet:
		value := []byte{byte(f.supported >> 8), byte(f.supported)}
		f.answer(transfer{protocol: protocolBusAccess, service: serviceFeatureResponse, body: append([]byte{t.body[0]}, value...)})

	case t.protocol == protocolBusAccess && t.service == serviceFeatureSet:
		if Feature(t.body[0]) == FeatureActiveEMI {
			f.activeEMI = t.body[1]
		}

	case t.protocol == protocolTunnel:
		if t.service != f.activeEMI {
			f.t.Errorf("message in EMI %d, active %d", t.service, f.activeEMI)
		}

		var msg cemi.Message
		if _, err := cemi.Unpack(t.body, &msg); err != nil {
			f.t.Errorf("unpack % x: %v", t.body, err)
			return len(p), nil
		}

		f.sent = append(f.sent, msg)

		var con cemi.Message

		switch msg := msg.(type) {
		case *cemi.MPropReadReq:
			c := &cemi.MPropReadCon{MProp: msg.MProp}
			c.Data = []byte{f.commMode}
			con = c

		case *cemi.MPropWriteReq:
			f.commMode = msg.Data[0]
			con = &cemi.MPropWriteCon{MProp: msg.MProp}

		case *cemi.LDataReq:
			c := &cemi.LDataCon{LData: msg.LData}
			c.Source = cemi.NewIndividualAddr3(1, 1, 250)
			con = c
		}

		body := make([]byte, cemi.Size(con))
		cemi.Pack(body, con)
		f.answer(transfer{protocol: protocolTunnel, service: t.service, body: body})
	}

	return len(p), nil
}

func TestOpen(t *testing.T) {
	f := newFakeInterface(t, 0x0007)

	l, err := Open(f, ft12.DefaultConfig)
	if err != nil {
		t.Fatal(err)
	}

	if l.Protocol() != ft12.ProtocolCEMI || f.activeEMI != byte(CEMI) {
		t.Fatalf("protocol %v, active EMI %d", l.Protocol(), f.activeEMI)
	}

	err = l.Send(&cemi.LDataReq{LData: cemi.LData{
		Control1:    cemi.Control1Request(cemi.PrioLow),
		Control2:    cemi.Control2GroupAddr | cemi.Control2Hops(6),
		Destination: uint16(cemi.NewGroupAddr3(1, 0, 1)),
		Data:        &cemi.AppData{Command: cemi.GroupValueRead},
	}})
	if err != nil {
		t.Fatal(err)
	}

	if l.Address() != cemi.NewIndividualAddr3(1, 1, 250) {
		t.Fatalf("address %v", l.Address())
	}

	// A frame from the bus.
	ind := &cemi.LDataInd{LData: cemi.LData{
		Control1:    cemi.Control1Request(cemi.PrioLow),
		Control2:    cemi.Control2GroupAddr | cemi.Control2Hops(6),
		Source:      cemi.NewIndividualAddr3(1, 1, 1),
		Destination: uint16(cemi.NewGroupAddr3(1, 0, 5)),
		Data:        &cemi.AppData{Command: cemi.GroupValueResponse, Data: []byte{0, 0x0c, 0x7e}},
	}}
	body := make([]byte, cemi.Size(ind))
	cemi.Pack(body, ind)
	f.answer(transfer{protocol: protocolTunnel, service: byte(CEMI), body: body})

	got := <-l.Inbound()
	if got.Source != ind.Source || got.Destination != ind.Destination {
		t.Fatalf("got %+v", got)
	}

	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	if _, open := <-l.Inbound(); open {
		t.Fatal("inbound still open")
	}
}

func TestOpenEMI1(t *testing.T) {
	f := newFakeInterface(t, 0x0001)

	if _, err := Open(f, ft12.DefaultConfig); err == nil {
		t.Fatal("opened an EMI1 interface")
	}
}
