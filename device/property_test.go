package device

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/shabunin/tiny-knx-go/apdu"
	"github.com/shabunin/tiny-knx-go/cemi"
	"github.com/shabunin/tiny-knx-go/transport"
)

// testMemory is 16 bytes at 0x4000.
type testMemory [16]byte

func (m *testMemory) ReadMemory(address uint32, data []byte) bool {
	if address < 0x4000 || address+uint32(len(data)) > 0x4000+uint32(len(m)) {
		return false
	}

	copy(data, m[address-0x4000:])

	return true
}

func (m *testMemory) WriteMemory(address uint32, data []byte) bool {
	if address < 0x4000 || address+uint32(len(data)) > 0x4000+uint32(len(m)) {
		return false
	}

	copy(m[address-0x4000:], data)

	return true
}

func connect(t *testing.T, tb *testBus) *transport.Client {
	t.Helper()

	client := transport.NewClient(tb.bus.Attach(toolAddr), transport.DefaultConfig)
	t.Cleanup(client.Close)

	if err := client.Connect(deviceAddr); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	return client
}

func writeProperty(t *testing.T, client *transport.Client, ref apdu.PropertyRef, data []byte) apdu.PropertyValueResponse {
	t.Helper()

	response, err := client.Request(apdu.PropertyValueWrite{PropertyRef: ref, Data: data}, func(a apdu.APDU) bool {
		_, ok := a.(apdu.PropertyValueResponse)
		return ok
	})
	if err != nil {
		t.Fatalf("PropertyValueWrite: %v", err)
	}

	return response.(apdu.PropertyValueResponse)
}

func TestInterfaceObject(t *testing.T) {
	tb := startDevice(t, switchActuator(deviceAddr))

	// An array property with up to 8 elements of 2 bytes.
	var values []byte

	index := tb.device.AddInterfaceObject(InterfaceObject{Type: 0x1234, Properties: []Property{{
		ID: 200, Type: pdtUnsignedInt, MaxElements: 8, ReadLevel: 3, WriteLevel: 2,
		Elements: func() uint16 { return uint16(len(values) / 2) },
		Read: func(start uint16, count uint8) []byte {
			return values[2*(start-1) : 2*(int(start)-1+int(count))]
		},
		Write: func(start uint16, count uint8, data []byte) bool {
			if start == 0 || len(data) != 2*int(count) || 2*(int(start)-1) > len(values) {
				return false
			}

			values = append(values[:2*(start-1)], data...)

			return true
		},
	}}})
	if index != 1 {
		t.Fatalf("Index %d", index)
	}

	client := connect(t, tb)

	if value, err := client.ReadProperty(1, pidObjectType, 1, 1); err != nil || !bytes.Equal(value, []byte{0x12, 0x34}) {
		t.Errorf("Object type % x, %v", value, err)
	}

	ref := apdu.PropertyRef{Object: 1, PropertyID: 200, Count: 3, Start: 1}

	if r := writeProperty(t, client, ref, []byte{0, 1, 0, 2, 0, 3}); r.Count != 3 || !bytes.Equal(r.Data, []byte{0, 1, 0, 2, 0, 3}) {
		t.Errorf("Write answered with %+v", r)
	}

	if count, err := client.ReadProperty(1, 200, 0, 1); err != nil || !bytes.Equal(count, []byte{0, 3}) {
		t.Errorf("Element count % x, %v", count, err)
	}

	if value, err := client.ReadProperty(1, 200, 2, 2); err != nil || !bytes.Equal(value, []byte{0, 2, 0, 3}) {
		t.Errorf("Elements 2–3: % x, %v", value, err)
	}

	// Beyond the current elements, beyond the maximum, rejected by Write.
	if _, err := client.ReadProperty(1, 200, 3, 2); !errors.Is(err, transport.ErrRejected) {
		t.Errorf("Reading beyond the elements: %v", err)
	}

	if r := writeProperty(t, client, apdu.PropertyRef{Object: 1, PropertyID: 200, Count: 2, Start: 8}, []byte{0, 0, 0, 0}); r.Count != 0 {
		t.Errorf("Writing beyond the maximum: %+v", r)
	}

	if r := writeProperty(t, client, apdu.PropertyRef{Object: 1, PropertyID: 200, Count: 1, Start: 1}, []byte{0}); r.Count != 0 {
		t.Errorf("Write rejected by the property: %+v", r)
	}

	// Descriptions by ID and by index.
	for _, request := range []apdu.PropertyDescriptionRead{{Object: 1, PropertyID: 200}, {Object: 1, Index: 1}} {
		d, err := client.DescribeProperty(request.Object, request.PropertyID, request.Index)
		if err != nil || d.PropertyID != 200 || d.Index != 1 || d.Type != pdtUnsignedInt || d.MaxElements != 8 ||
			!d.WriteEnable || d.ReadLevel != 3 || d.WriteLevel != 2 {
			t.Errorf("Description %+v, %v", d, err)
		}
	}

	if d, err := client.DescribeProperty(1, 0, 2); err != nil || d.MaxElements != 0 {
		t.Errorf("Description past the last property: %+v, %v", d, err)
	}

	// The device object lists the interface objects.
	if list, err := client.ReadProperty(0, pidIOList, 1, 2); err != nil || !bytes.Equal(list, []byte{0, 0, 0x12, 0x34}) {
		t.Errorf("IO_LIST = % x, %v", list, err)
	}
}

func TestMemory(t *testing.T) {
	tb := startDevice(t, switchActuator(deviceAddr))

	memory := &testMemory{}
	tb.device.SetMemory(memory)

	client := connect(t, tb)

	if _, err := client.Request(apdu.MemoryWrite{Address: 0x4002, Data: []byte{1, 2, 3}}, nil); err != nil {
		t.Fatalf("MemoryWrite: %v", err)
	}

	if data, err := client.ReadMemory(0x4001, 4); err != nil || !bytes.Equal(data, []byte{0, 1, 2, 3}) {
		t.Errorf("MemoryRead: % x, %v", data, err)
	}

	if _, err := client.ReadMemory(0x3FFF, 2); !errors.Is(err, transport.ErrRejected) {
		t.Errorf("Reading outside the memory: %v", err)
	}

	// In verify mode, a write is answered with the memory.
	writeProperty(t, client, apdu.PropertyRef{PropertyID: pidDeviceControl, Count: 1, Start: 1}, []byte{deviceControlVerify})

	response, err := client.Request(apdu.MemoryWrite{Address: 0x4000, Data: []byte{9}}, func(a apdu.APDU) bool {
		_, ok := a.(apdu.MemoryResponse)
		return ok
	})
	if err != nil || !bytes.Equal(response.(apdu.MemoryResponse).Data, []byte{9}) {
		t.Errorf("Verified write: %+v, %v", response, err)
	}

	// Extended memory services.
	response, err = client.Request(apdu.MemoryExtWrite{Address: 0x400E, Data: []byte{7, 8}}, func(a apdu.APDU) bool {
		_, ok := a.(apdu.MemoryExtWriteResponse)
		return ok
	})
	if r, _ := response.(apdu.MemoryExtWriteResponse); err != nil || r.Code != apdu.ReturnSuccess || r.Address != 0x400E {
		t.Errorf("MemoryExtWrite: %+v, %v", response, err)
	}

	for _, c := range []struct {
		address uint32
		code    uint8
		data    []byte
	}{
		{0x400D, apdu.ReturnSuccess, []byte{0, 7, 8}},
		{0x400F, apdu.ReturnMemoryError, nil},
	} {
		response, err = client.Request(apdu.MemoryExtRead{Count: 3, Address: c.address}, func(a apdu.APDU) bool {
			_, ok := a.(apdu.MemoryExtReadResponse)
			return ok
		})
		if r, _ := response.(apdu.MemoryExtReadResponse); err != nil || r.Code != c.code || !bytes.Equal(r.Data, c.data) {
			t.Errorf("MemoryExtRead at %#x: %+v, %v", c.address, response, err)
		}
	}

	if !bytes.Equal(memory[:3], []byte{9, 0, 1}) || !bytes.Equal(memory[14:], []byte{7, 8}) {
		t.Errorf("Memory % x", memory[:])
	}
}

func TestBind(t *testing.T) {
	tb := startDevice(t, switchActuator(deviceAddr))

	changed := make(chan []byte, 1)
	tb.device.Object("switch").OnChange(func(o *Object) { changed <- o.Value() })

	newGA := cemi.NewGroupAddr3(2, 0, 1)
	initGA := cemi.NewGroupAddr3(2, 0, 2)

	// The switch moves to a new address; the status reads its value from the bus.
	tb.device.Bind([]Binding{
		{Flags: FlagCommunicate | FlagWrite, Addresses: []cemi.GroupAddr{newGA, 0}},
		{Flags: FlagCommunicate | FlagInit | FlagUpdate, Addresses: []cemi.GroupAddr{initGA}},
	})

	if ind, a := receive(t, tb.sender); cemi.GroupAddr(ind.Destination) != initGA {
		t.Errorf("Init read sent to %v: %+v", cemi.GroupAddr(ind.Destination), a)
	} else if _, ok := a.(apdu.GroupValueRead); !ok {
		t.Errorf("Init read is %+v", a)
	}

	// The old address no longer reaches the switch: the first change is the second write.
	tb.sendGroup(tb.sender, switchGA, apdu.GroupValueWrite{Data: []byte{1}})
	tb.sendGroup(tb.sender, newGA, apdu.GroupValueWrite{Data: []byte{0}})

	select {
	case v := <-changed:
		if !bytes.Equal(v, []byte{0}) {
			t.Errorf("Switch received % x", v)
		}
	case <-time.After(time.Second):
		t.Fatal("Switch not changed")
	}

	config := tb.device.Config()
	if len(config.Objects[0].Addresses) != 1 || config.Objects[0].Addresses[0] != newGA || config.Objects[0].Name != "switch" {
		t.Errorf("Config %+v", config.Objects[0])
	}

	// Objects without a binding stop communicating.
	tb.device.Bind(nil)

	if f := tb.device.Object("status").Flags(); f != 0 {
		t.Errorf("Status flags %v", f)
	}
}
