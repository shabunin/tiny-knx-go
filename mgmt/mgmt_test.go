package mgmt

import (
	"bytes"
	"testing"
	"time"

	"github.com/shabunin/tiny-knx-go/apdu"
	"github.com/shabunin/tiny-knx-go/cemi"
	"github.com/shabunin/tiny-knx-go/device"
	"github.com/shabunin/tiny-knx-go/link/mock"
	"github.com/shabunin/tiny-knx-go/transport"
)

var toolAddr = cemi.NewIndividualAddr3(1, 1, 240)

// startDevice runs a device on the bus until the test ends.
func startDevice(t *testing.T, bus *mock.Bus, addr cemi.IndividualAddr, mask uint16) {
	t.Helper()

	d, err := device.New(device.Identity{
		MaskVersion:  mask,
		SerialNumber: apdu.SerialNumber{0x00, 0xfa, 0, 0, 0, byte(addr)},
		OrderNumber:  "TEST",
	}, device.Config{Address: addr})
	if err != nil {
		t.Fatal(err)
	}

	l := bus.Attach(addr)
	done := make(chan struct{})

	go func() {
		d.Run(l)
		close(done)
	}()

	t.Cleanup(func() {
		l.Close()
		<-done
	})
}

var fastScan = ScanConfig{
	Transport: transport.Config{
		AckTimeout:     50 * time.Millisecond,
		MaxRepetitions: 1,
	},
	ResponseTimeout: 100 * time.Millisecond,
}

func testScan(t *testing.T, confirmAll bool) {
	bus := mock.NewBus()
	bus.ConfirmAll = confirmAll

	startDevice(t, bus, cemi.NewIndividualAddr3(1, 1, 10), 0x07B0)
	startDevice(t, bus, cemi.NewIndividualAddr3(1, 1, 20), 0x0701)
	startDevice(t, bus, cemi.NewIndividualAddr3(1, 2, 10), 0x07B0) // other line

	// A device that acknowledges frames on the bus but never answers is not reported.
	bus.Attach(cemi.NewIndividualAddr3(1, 1, 30))

	tool := bus.Attach(toolAddr)
	defer tool.Close()

	var reported []Found

	config := fastScan
	config.OnFound = func(f Found) { reported = append(reported, f) }

	start := time.Now()

	found, err := Scan(tool, LineAddresses(1, 1), config)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("Scanned 256 addresses in %v", time.Since(start))

	expected := []Found{
		{Address: cemi.NewIndividualAddr3(1, 1, 10), Mask: 0x07B0},
		{Address: cemi.NewIndividualAddr3(1, 1, 20), Mask: 0x0701},
	}

	if len(found) != len(expected) || len(reported) != len(expected) {
		t.Fatalf("Found %+v, reported %+v", found, reported)
	}

	for i := range expected {
		if found[i] != expected[i] {
			t.Errorf("Found %+v, expected %+v", found[i], expected[i])
		}
	}
}

func TestScanWithAcknowledgements(t *testing.T) {
	testScan(t, false)
}

func TestScanWithoutAcknowledgements(t *testing.T) {
	testScan(t, true)
}

func TestReadProperties(t *testing.T) {
	bus := mock.NewBus()
	addr := cemi.NewIndividualAddr3(1, 1, 10)
	startDevice(t, bus, addr, 0x07B0)

	client := transport.NewClient(bus.Attach(toolAddr), transport.DefaultConfig)
	defer client.Close()

	if err := client.Connect(addr); err != nil {
		t.Fatal(err)
	}

	properties, err := ReadProperties(client, PropertyOptions{Values: true})
	if err != nil {
		t.Fatal(err)
	}

	byName := make(map[string]Property)

	for _, p := range properties {
		if p.Object != 0 || p.ObjectType != 0 || p.Err != nil {
			t.Errorf("Property %+v", p)
		}

		byName[PropertyName(p.ObjectType, p.PropertyID)] = p
	}

	if len(properties) != 15 {
		t.Errorf("Read %d properties", len(properties))
	}

	checks := []struct {
		name  string
		value []byte
	}{
		{"OBJECT_TYPE", []byte{0, 0}},
		{"SERIAL_NUMBER", []byte{0x00, 0xfa, 0, 0, 0, 10}},
		{"ORDER_INFO", []byte("TEST\x00\x00\x00\x00\x00\x00")},
		{"DEVICE_DESCRIPTOR", []byte{0x07, 0xb0}},
		{"DEVICE_ADDR", []byte{10}},
	}

	for _, c := range checks {
		if p, ok := byName[c.name]; !ok || p.Elements != 1 || !bytes.Equal(p.Value, c.value) {
			t.Errorf("%s: %+v, expected % x", c.name, p, c.value)
		}
	}

	if p := byName["PROGMODE"]; !p.WriteEnable || p.Type != 0x33 {
		t.Errorf("PROGMODE description %+v", p.PropertyDescriptionResponse)
	}
}

// Zennio devices leave the property ID out of the descriptions by index, and reject reading the
// number of elements.
func TestReadPropertiesWithoutIDs(t *testing.T) {
	bus := mock.NewBus()
	addr := cemi.NewIndividualAddr3(1, 1, 10)
	startDevice(t, bus, addr, 0x0701)

	byIndex := false

	bus.Drop = func(from cemi.IndividualAddr, frame *cemi.LData) bool {
		app, ok := frame.Data.(*cemi.AppData)
		if !ok || app.Command != cemi.Escape || len(app.Data) < 4 {
			return false
		}

		switch app.Data[0] {
		case 0xD8: // PropertyDescription_Read
			byIndex = app.Data[2] == 0
		case 0xD9: // PropertyDescription_Response
			if byIndex {
				app.Data[2] = 0
			}
		case 0xD6: // PropertyResponse
			if len(app.Data) >= 5 && app.Data[3]&0x0F == 0 && app.Data[4] == 0 {
				app.Data = app.Data[:5]
				app.Data[3] = 0 // no elements: rejected
			}
		}

		return false
	}

	client := transport.NewClient(bus.Attach(toolAddr), transport.DefaultConfig)
	defer client.Close()

	if err := client.Connect(addr); err != nil {
		t.Fatal(err)
	}

	properties, err := ReadProperties(client, PropertyOptions{Values: true})
	if err != nil {
		t.Fatal(err)
	}

	if len(properties) != 15 {
		t.Errorf("Read %d properties", len(properties))
	}

	for _, p := range properties {
		if p.PropertyID == 0 || p.Err != nil {
			t.Errorf("Property %+v", p)
		}

		if p.PropertyID == 11 && (p.Elements != 1 || !bytes.Equal(p.Value, []byte{0x00, 0xfa, 0, 0, 0, 10})) {
			t.Errorf("Serial number %d elements, % x", p.Elements, p.Value)
		}
	}
}

func TestNames(t *testing.T) {
	cases := []struct{ got, want string }{
		{PropertyName(0, 11), "SERIAL_NUMBER"},
		{PropertyName(0, 54), "PROGMODE"},
		{PropertyName(8, 54), "PID 54"},
		{PropertyName(0, 200), "PID 200"},
		{ObjectTypeName(8), "cEMI server"},
		{ObjectTypeName(99), "object type 99"},
	}

	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("Got %q, want %q", c.got, c.want)
		}
	}

	if PDTSize(0x16) != 6 || PDTSize(0x04) != 2 || PDTSize(0x10) != 0 {
		t.Error("PDTSize is wrong")
	}
}
