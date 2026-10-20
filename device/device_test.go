package device

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/shabunin/tiny-knx-go/apdu"
	"github.com/shabunin/tiny-knx-go/cemi"
	"github.com/shabunin/tiny-knx-go/dpt"
	"github.com/shabunin/tiny-knx-go/link/mock"
	"github.com/shabunin/tiny-knx-go/transport"
)

var (
	testIdentity = Identity{
		SerialNumber:     apdu.SerialNumber{0x00, 0xfa, 0x01, 0x02, 0x03, 0x04},
		OrderNumber:      "TINY-01",
		HardwareType:     [6]byte{0, 0, 0, 0, 0, 1},
		FirmwareRevision: 3,
	}

	switchGA   = cemi.NewGroupAddr3(1, 1, 1)
	statusGA   = cemi.NewGroupAddr3(1, 1, 2)
	deviceAddr = cemi.NewIndividualAddr3(1, 1, 50)
	senderAddr = cemi.NewIndividualAddr3(1, 1, 1)
	toolAddr   = cemi.NewIndividualAddr3(1, 1, 240)
	bcastAddr  = cemi.NewIndividualAddr3(1, 1, 241)
)

func switchActuator(addr cemi.IndividualAddr) Config {
	return Config{
		Address: addr,
		Objects: []ObjectConfig{
			{Name: "switch", DPT: "1.001", Flags: FlagCommunicate | FlagWrite, Addresses: []cemi.GroupAddr{switchGA}},
			{Name: "status", DPT: "1.001", Flags: FlagCommunicate | FlagRead | FlagTransmit, Addresses: []cemi.GroupAddr{statusGA}},
		},
	}
}

type testBus struct {
	t      *testing.T
	bus    *mock.Bus
	device *Device
	sender *mock.Link // a plain bus device
}

func startDevice(t *testing.T, config Config) *testBus {
	t.Helper()

	bus := mock.NewBus()

	d, err := New(testIdentity, config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	addr := config.Address
	if addr == 0 {
		addr = DefaultAddress
	}

	l := bus.Attach(addr)
	done := make(chan struct{})

	go func() {
		d.Run(l)
		close(done)
	}()

	sender := bus.Attach(senderAddr)

	t.Cleanup(func() {
		l.Close()
		<-done
		sender.Close()
	})

	return &testBus{t: t, bus: bus, device: d, sender: sender}
}

func (tb *testBus) sendGroup(from *mock.Link, ga cemi.GroupAddr, a apdu.APDU) {
	tb.t.Helper()

	err := from.Send(&cemi.LDataReq{LData: cemi.LData{
		Control1:    cemi.Control1Request(cemi.PrioLow),
		Control2:    cemi.Control2GroupAddr | cemi.Control2Hops(6),
		Destination: uint16(ga),
		Data:        apdu.Encode(a),
	}})
	if err != nil {
		tb.t.Fatalf("Send: %v", err)
	}
}

// receive waits for the next frame on l and decodes its APDU.
func receive(t *testing.T, l *mock.Link) (*cemi.LDataInd, apdu.APDU) {
	t.Helper()

	select {
	case ind := <-l.Inbound():
		app, ok := ind.Data.(*cemi.AppData)
		if !ok {
			return ind, nil
		}

		a, err := apdu.Decode(app)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}

		return ind, a

	case <-time.After(time.Second):
		t.Fatal("No frame received")
	}

	return nil, nil
}

func expectNothing(t *testing.T, l *mock.Link) {
	t.Helper()

	select {
	case ind := <-l.Inbound():
		t.Errorf("Unexpected frame %+v", ind.LData)
	case <-time.After(50 * time.Millisecond):
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("Timed out waiting for %s", what)
		}

		time.Sleep(time.Millisecond)
	}
}

func TestGroupWrite(t *testing.T) {
	tb := startDevice(t, switchActuator(deviceAddr))
	sw := tb.device.Object("switch")

	changed := make(chan bool, 1)
	sw.OnChange(func(o *Object) {
		var v dpt.DPT_1001
		o.Get(&v)
		changed <- bool(v)
	})

	tb.sendGroup(tb.sender, switchGA, apdu.GroupValueWrite{Data: dpt.DPT_1001(true).Pack()})

	select {
	case on := <-changed:
		if !on {
			t.Error("Switch reported off")
		}
	case <-time.After(time.Second):
		t.Fatal("OnChange not called")
	}

	// The status object has no W flag: writes to it are ignored.
	tb.sendGroup(tb.sender, statusGA, apdu.GroupValueWrite{Data: dpt.DPT_1001(true).Pack()})
	expectNothing(t, tb.sender)

	if v := tb.device.Object("status").Value(); !bytes.Equal(v, []byte{0}) {
		t.Errorf("Status changed to % x by a write", v)
	}
}

func TestGroupRead(t *testing.T) {
	tb := startDevice(t, switchActuator(deviceAddr))
	tb.device.Object("status").Store(dpt.DPT_1001(true))

	tb.sendGroup(tb.sender, statusGA, apdu.GroupValueRead{})

	ind, a := receive(t, tb.sender)
	response, ok := a.(apdu.GroupValueResponse)

	if !ok || !bytes.Equal(response.Data, []byte{1}) || ind.Source != deviceAddr ||
		cemi.GroupAddr(ind.Destination) != statusGA {
		t.Errorf("Got %+v from %v to %v", a, ind.Source, cemi.GroupAddr(ind.Destination))
	}

	// The switch object has no R flag.
	tb.sendGroup(tb.sender, switchGA, apdu.GroupValueRead{})
	expectNothing(t, tb.sender)
}

func TestSetTransmits(t *testing.T) {
	tb := startDevice(t, switchActuator(deviceAddr))

	tb.device.Object("status").Set(dpt.DPT_1001(true))

	ind, a := receive(t, tb.sender)
	if write, ok := a.(apdu.GroupValueWrite); !ok || !bytes.Equal(write.Data, []byte{1}) ||
		cemi.GroupAddr(ind.Destination) != statusGA || ind.Control1 != cemi.Control1Request(cemi.PrioLow) {
		t.Errorf("Got %+v to %v with control %#x", a, cemi.GroupAddr(ind.Destination), ind.Control1)
	}

	// No T flag: the value is stored but not sent.
	tb.device.Object("switch").Set(dpt.DPT_1001(true))
	expectNothing(t, tb.sender)
}

func TestUpdateAndInitFlags(t *testing.T) {
	config := Config{
		Address: deviceAddr,
		Objects: []ObjectConfig{{
			Name:      "temperature",
			DPT:       "9.001",
			Flags:     FlagCommunicate | FlagUpdate | FlagInit,
			Addresses: []cemi.GroupAddr{statusGA},
		}},
	}

	tb := startDevice(t, config)

	// The I flag makes the device ask for the value at start ...
	if _, a := receive(t, tb.sender); a != (apdu.GroupValueRead{}) {
		t.Fatalf("Expected GroupValueRead at start, got %+v", a)
	}

	// ... and the U flag accepts the answer.
	tb.sendGroup(tb.sender, statusGA, apdu.GroupValueResponse{Data: dpt.DPT_9001(21.5).Pack()})

	temperature := tb.device.Object("temperature")
	eventually(t, "the temperature", func() bool {
		var v dpt.DPT_9001
		return temperature.Get(&v) == nil && v == 21.5
	})
}

// broadcast sends a system broadcast and returns the responses that arrive.
func broadcast(t *testing.T, l *mock.Link, a apdu.APDU) []*cemi.LDataInd {
	t.Helper()

	err := l.Send(&cemi.LDataReq{LData: cemi.LData{
		Control1:    cemi.Control1Request(cemi.PrioSystem),
		Control2:    cemi.Control2GroupAddr | cemi.Control2Hops(6),
		Destination: 0,
		Data:        apdu.Encode(a),
	}})
	if err != nil {
		t.Fatalf("Broadcast: %v", err)
	}

	var responses []*cemi.LDataInd

	for {
		select {
		case ind := <-l.Inbound():
			responses = append(responses, ind)
		case <-time.After(100 * time.Millisecond):
			return responses
		}
	}
}

// TestProgramIndividualAddress follows what ETS does to give a new device its address.
func TestProgramIndividualAddress(t *testing.T) {
	tb := startDevice(t, switchActuator(0))
	tool := tb.bus.Attach(bcastAddr)
	newAddr := cemi.NewIndividualAddr3(1, 1, 50)

	if got := tb.device.Address(); got != DefaultAddress {
		t.Fatalf("Unprogrammed device has address %v", got)
	}

	addressChanged := make(chan cemi.IndividualAddr, 1)
	tb.device.OnAddressChange(func(addr cemi.IndividualAddr) { addressChanged <- addr })

	progModeChanges := make(chan bool, 4)
	tb.device.OnProgModeChange(func(on bool) { progModeChanges <- on })

	restarted := make(chan struct{}, 1)
	tb.device.OnRestart(func() { restarted <- struct{}{} })

	// Outside programming mode the device keeps quiet.
	if responses := broadcast(t, tool, apdu.IndividualAddressRead{}); len(responses) != 0 {
		t.Fatalf("Device answered outside programming mode: %+v", responses[0].LData)
	}

	// The user presses the programming button.
	tb.device.SetProgMode(true)

	if on := <-progModeChanges; !on {
		t.Fatal("Programming mode not reported")
	}

	responses := broadcast(t, tool, apdu.IndividualAddressRead{})
	if len(responses) != 1 || responses[0].Source != DefaultAddress {
		t.Fatalf("Expected one IndividualAddressResponse from %v, got %d", DefaultAddress, len(responses))
	}

	broadcast(t, tool, apdu.IndividualAddressWrite{Address: newAddr})

	select {
	case addr := <-addressChanged:
		if addr != newAddr {
			t.Errorf("Address changed to %v, expected %v", addr, newAddr)
		}
	case <-time.After(time.Second):
		t.Fatal("OnAddressChange not called")
	}

	// ETS verifies the new address, connects and restarts the device.
	responses = broadcast(t, tool, apdu.IndividualAddressRead{})
	if len(responses) != 1 || responses[0].Source != newAddr {
		t.Fatalf("Verification failed: %d responses", len(responses))
	}

	client := transport.NewClient(tb.bus.Attach(toolAddr), transport.DefaultConfig)
	defer client.Close()

	if err := client.Connect(newAddr); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	if mask, err := client.DeviceDescriptor(); err != nil || mask != DefaultMaskVersion {
		t.Errorf("DeviceDescriptor = %#04x, %v", mask, err)
	}

	if _, err := client.Request(apdu.Restart{}, nil); err != nil {
		t.Errorf("Restart: %v", err)
	}

	select {
	case <-restarted:
	case <-time.After(time.Second):
		t.Fatal("OnRestart not called")
	}

	if on := <-progModeChanges; on {
		t.Error("Programming mode still on after restart")
	}

	if got := tb.device.Config().Address; got != newAddr {
		t.Errorf("Config address %v, expected %v", got, newAddr)
	}
}

func TestDeviceProperties(t *testing.T) {
	tb := startDevice(t, switchActuator(deviceAddr))

	client := transport.NewClient(tb.bus.Attach(toolAddr), transport.DefaultConfig)
	defer client.Close()

	if err := client.Connect(deviceAddr); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	properties := []struct {
		id    uint8
		value []byte
	}{
		{pidObjectType, []byte{0, 0}},
		{pidFirmwareRevision, []byte{3}},
		{pidSerialNumber, testIdentity.SerialNumber[:]},
		{pidManufacturerID, []byte{0x00, 0xfa}},
		{pidOrderInfo, []byte("TINY-01\x00\x00\x00")},
		{pidProgMode, []byte{0}},
		{pidMaxAPDULength, []byte{0, 15}},
		{pidSubnetAddr, []byte{0x11}},
		{pidDeviceAddr, []byte{50}},
		{pidHardwareType, []byte{0, 0, 0, 0, 0, 1}},
		{pidDeviceDescriptor, []byte{0x07, 0xb0}},
		{pidDeviceControl, []byte{0}},
		{pidVersion, []byte{0, 3}},
		{pidRoutingCount, []byte{0x60}},
		{pidIOList, []byte{0, 0}},
	}

	for _, p := range properties {
		if value, err := client.ReadProperty(0, p.id, 1, 1); err != nil || !bytes.Equal(value, p.value) {
			t.Errorf("Property %d = % x, %v; expected % x", p.id, value, err, p.value)
		}
	}

	if count, err := client.ReadProperty(0, pidSerialNumber, 0, 1); err != nil || !bytes.Equal(count, []byte{0, 1}) {
		t.Errorf("Element count = % x, %v", count, err)
	}

	if _, err := client.ReadProperty(0, 200, 1, 1); !errors.Is(err, transport.ErrRejected) {
		t.Errorf("Unknown property: %v", err)
	}

	if _, err := client.ReadProperty(4, pidSerialNumber, 1, 1); !errors.Is(err, transport.ErrRejected) {
		t.Errorf("Unknown object: %v", err)
	}

	// ETS can switch programming mode through the property.
	write := apdu.PropertyValueWrite{
		PropertyRef: apdu.PropertyRef{Object: 0, PropertyID: pidProgMode, Count: 1, Start: 1},
		Data:        []byte{1},
	}

	response, err := client.Request(write, func(a apdu.APDU) bool {
		_, ok := a.(apdu.PropertyValueResponse)
		return ok
	})
	if err != nil || !bytes.Equal(response.(apdu.PropertyValueResponse).Data, []byte{1}) || !tb.device.ProgMode() {
		t.Errorf("Writing programming mode: %+v, %v", response, err)
	}

	// Other properties are read-only.
	write.PropertyID = pidSerialNumber

	response, err = client.Request(write, func(a apdu.APDU) bool {
		_, ok := a.(apdu.PropertyValueResponse)
		return ok
	})
	if err != nil || response.(apdu.PropertyValueResponse).Count != 0 {
		t.Errorf("Writing the serial number was not rejected: %+v, %v", response, err)
	}

	description, err := client.Request(apdu.PropertyDescriptionRead{PropertyID: pidProgMode}, func(a apdu.APDU) bool {
		_, ok := a.(apdu.PropertyDescriptionResponse)
		return ok
	})
	if d, ok := description.(apdu.PropertyDescriptionResponse); err != nil || !ok || !d.WriteEnable ||
		d.Type != pdtBitset8 || d.MaxElements != 1 {
		t.Errorf("Description of programming mode: %+v, %v", description, err)
	}

	if _, err := client.ReadMemory(0x0060, 1); !errors.Is(err, transport.ErrRejected) {
		t.Errorf("Memory read: %v", err)
	}
}

func TestSerialNumberAddressing(t *testing.T) {
	tb := startDevice(t, switchActuator(deviceAddr))
	tool := tb.bus.Attach(bcastAddr)

	other := apdu.SerialNumber{0x00, 0xfa, 9, 9, 9, 9}
	if responses := broadcast(t, tool, apdu.IndividualAddressSerialNumberRead{SerialNumber: other}); len(responses) != 0 {
		t.Error("Device answered for another serial number")
	}

	responses := broadcast(t, tool, apdu.IndividualAddressSerialNumberRead{SerialNumber: testIdentity.SerialNumber})
	if len(responses) != 1 || responses[0].Source != deviceAddr {
		t.Fatalf("Expected a response from %v, got %d", deviceAddr, len(responses))
	}

	// Works without programming mode.
	newAddr := cemi.NewIndividualAddr3(1, 2, 3)
	broadcast(t, tool, apdu.IndividualAddressSerialNumberWrite{SerialNumber: testIdentity.SerialNumber, Address: newAddr})

	eventually(t, "the new address", func() bool { return tb.device.Address() == newAddr })
}

func TestConnectionlessRequest(t *testing.T) {
	tb := startDevice(t, switchActuator(deviceAddr))
	tool := tb.bus.Attach(toolAddr)

	err := tool.Send(&cemi.LDataReq{LData: cemi.LData{
		Control1:    cemi.Control1Request(cemi.PrioLow),
		Control2:    cemi.Control2Hops(6),
		Destination: uint16(deviceAddr),
		Data:        apdu.Encode(apdu.DeviceDescriptorRead{}),
	}})
	if err != nil {
		t.Fatal(err)
	}

	ind, a := receive(t, tool)
	if response, ok := a.(apdu.DeviceDescriptorResponse); !ok || !bytes.Equal(response.Descriptor, []byte{0x07, 0xb0}) ||
		ind.Data.(*cemi.AppData).Numbered || cemi.IndividualAddr(ind.Destination) != toolAddr {
		t.Errorf("Got %+v", a)
	}
}

func TestIgnoresOtherDevices(t *testing.T) {
	tb := startDevice(t, switchActuator(deviceAddr))
	other := cemi.NewIndividualAddr3(1, 1, 99)
	tb.bus.Attach(other)

	config := transport.DefaultConfig
	config.AckTimeout = 20 * time.Millisecond
	config.MaxRepetitions = 1

	client := transport.NewClient(tb.bus.Attach(toolAddr), config)
	defer client.Close()

	// The device sees the connection to another device but must not answer it.
	if err := client.Connect(other); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	if _, err := client.DeviceDescriptor(); !errors.Is(err, transport.ErrDisconnected) {
		t.Errorf("DeviceDescriptor from a device that does not answer: %v", err)
	}
}

func TestConfigJSON(t *testing.T) {
	text := `{
		"address": "1.1.50",
		"objects": [
			{"name": "switch", "dpt": "1.001", "flags": "CW", "addresses": ["1/1/1", "1/1/10"]},
			{"name": "status", "dpt": "1.001", "flags": "c-r-t", "addresses": ["1/1/2"]}
		]
	}`

	var config Config
	if err := json.Unmarshal([]byte(text), &config); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if config.Address != deviceAddr || len(config.Objects) != 2 ||
		config.Objects[0].Flags != FlagCommunicate|FlagWrite ||
		config.Objects[1].Flags != FlagCommunicate|FlagRead|FlagTransmit ||
		config.Objects[0].Addresses[1] != cemi.NewGroupAddr3(1, 1, 10) {
		t.Errorf("Parsed %+v", config)
	}

	encoded, _ := json.Marshal(config)

	var again Config
	if err := json.Unmarshal(encoded, &again); err != nil {
		t.Fatalf("Unmarshal of %s: %v", encoded, err)
	}

	if again.Objects[1].Flags.String() != "CRT" {
		t.Errorf("Flags %s after round trip of %s", again.Objects[1].Flags, encoded)
	}

	if _, err := New(testIdentity, Config{Objects: []ObjectConfig{{DPT: "99.999"}}}); err == nil {
		t.Error("New accepted an unknown DPT")
	}

	var flags Flags
	if err := flags.UnmarshalText([]byte("CX")); err == nil {
		t.Error("Unknown flag accepted")
	}
}

// fixedAddressLink is a link whose address cannot be changed, like a KNXnet/IP tunnel.
type fixedAddressLink struct {
	*mock.Link
	SetAddress struct{} // hides mock.Link.SetAddress
}

func TestRunTakesLinkAddress(t *testing.T) {
	bus := mock.NewBus()
	tunnelAddr := cemi.NewIndividualAddr3(1, 1, 200)
	l := bus.Attach(tunnelAddr)

	d, err := New(testIdentity, switchActuator(deviceAddr))
	if err != nil {
		t.Fatal(err)
	}

	changed := make(chan cemi.IndividualAddr, 1)
	d.OnAddressChange(func(addr cemi.IndividualAddr) { changed <- addr })

	done := make(chan struct{})

	go func() {
		d.Run(fixedAddressLink{Link: l})
		close(done)
	}()

	defer func() {
		l.Close()
		<-done
	}()

	select {
	case addr := <-changed:
		if addr != tunnelAddr || d.Address() != tunnelAddr {
			t.Errorf("Device took address %v, expected %v", addr, tunnelAddr)
		}
	case <-time.After(time.Second):
		t.Fatal("Device kept its address")
	}
}
