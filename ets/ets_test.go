package ets

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/shabunin/tiny-knx-go/cemi"
	"github.com/shabunin/tiny-knx-go/device"
)

func TestMemory(t *testing.T) {
	var m memory

	a := m.alloc(6)
	b := m.alloc(8)

	if a == nil || b == nil || a.address != memoryStart || b.address != memoryStart+8 {
		t.Fatalf("Allocated %+v, %+v", a, b)
	}

	// A freed segment leaves a gap that the next allocation that fits reuses.
	m.free(a)

	if c := m.alloc(10); c == nil || c.address != memoryStart+16 {
		t.Errorf("Allocated %+v after the gap", c)
	}

	if c := m.alloc(4); c == nil || c.address != memoryStart {
		t.Errorf("Allocated %+v in the gap", c)
	}

	if m.alloc(MemorySize) != nil || m.alloc(0) != nil {
		t.Error("Allocated beyond the memory size")
	}

	copy(b.data, []byte{1, 2, 3, 4, 5, 6, 7, 8})

	if data := m.find(memoryStart+10, 4); !bytes.Equal(data, []byte{3, 4, 5, 6}) {
		t.Errorf("find = % x", data)
	}

	// Ranges must lie within one segment.
	if m.find(memoryStart+14, 4) != nil || m.find(0, 1) != nil {
		t.Error("find crossed a segment boundary")
	}

	if m.add(&segment{address: memoryStart + 12, data: make([]byte, 2)}) {
		t.Error("Added an overlapping segment")
	}

	if !m.add(&segment{address: 0x8000, data: make([]byte, 2)}) {
		t.Error("Could not add a free segment")
	}
}

func lsc(event uint8, rest ...byte) []byte {
	data := make([]byte, 10)
	data[0] = event
	copy(data[1:], rest)

	return data
}

func TestLoadStateMachine(t *testing.T) {
	var m memory

	tbl := &table{objectType: objectTypeApplication}

	steps := []struct {
		event []byte
		state uint8
		code  uint8
	}{
		{lsc(eventLoadCompleted), stateUnloaded, errNoFault},
		{lsc(eventStartLoading), stateLoading, errNoFault},
		{lsc(eventAdditionalLoadControls, 0x0B, 0, 0, 0, 8, 1, 0xAA), stateLoading, errNoFault},
		{lsc(eventLoadCompleted), stateLoaded, errNoFault},
		{lsc(eventAdditionalLoadControls, 0x0B, 0, 0, 0, 8, 0, 0), stateError, errInvalidOpcode},
		{lsc(eventStartLoading), stateError, errInvalidOpcode},
		{lsc(eventUnload), stateUnloaded, errNoFault},
		{lsc(eventStartLoading), stateLoading, errNoFault},
		{lsc(eventAdditionalLoadControls, 0x0A), stateError, errInvalidOpcode},
		{lsc(eventUnload), stateUnloaded, errNoFault},
		{lsc(9), stateError, errUndefinedLoadCmd},
	}

	for i, s := range steps {
		tbl.loadEvent(&m, s.event)

		if tbl.state != s.state || tbl.errorCode != s.code {
			t.Fatalf("Step %d (event % x): state %d, error %d; expected %d, %d",
				i, s.event[:2], tbl.state, tbl.errorCode, s.state, s.code)
		}

		// The relative allocation fills the segment if mode is 1.
		if i == 2 && (tbl.reference() != memoryStart || !bytes.Equal(tbl.segment.data, bytes.Repeat([]byte{0xAA}, 8))) {
			t.Fatalf("Segment %+v, reference %#x", tbl.segment, tbl.reference())
		}
	}

	if len(m.segments) != 0 || tbl.reference() != 0 {
		t.Errorf("Unloading left %d segments", len(m.segments))
	}

	// A group object table needs a segment.
	gos := &table{objectType: objectTypeGroupObjects}
	gos.loadEvent(&m, lsc(eventStartLoading))
	gos.loadEvent(&m, lsc(eventLoadCompleted))

	if gos.state != stateError || gos.errorCode != errSoftwareFault {
		t.Errorf("Group object table without a segment: state %d, error %d", gos.state, gos.errorCode)
	}

	// A segment larger than the memory cannot be allocated.
	tbl.loadEvent(&m, lsc(eventUnload))
	tbl.loadEvent(&m, lsc(eventStartLoading))
	tbl.loadEvent(&m, lsc(eventAdditionalLoadControls, 0x0B, 0, 1, 0, 0))

	if tbl.state != stateError || tbl.errorCode != errMaxTableLength {
		t.Errorf("Oversized segment: state %d, error %d", tbl.state, tbl.errorCode)
	}
}

func TestMCB(t *testing.T) {
	if crc := crc16([]byte("123456789")); crc != 0xE5CC {
		t.Errorf("crc16 = %#04x", crc)
	}

	tbl := &table{state: stateLoaded, segment: &segment{data: []byte("123456789")}}

	if mcb := tbl.mcb(); !bytes.Equal(mcb, []byte{0, 0, 0, 9, 0x00, 0xFF, 0xE5, 0xCC}) {
		t.Errorf("MCB = % x", mcb)
	}

	tbl.state = stateLoading
	if tbl.mcb() != nil {
		t.Error("MCB of a table that is not loaded")
	}
}

func TestDescriptorFlags(t *testing.T) {
	cases := []struct {
		descriptor uint16
		flags      string
	}{
		{0x4F07, "CRT"}, // knx-demo objects 1–3: 2 bytes, read, transmit, low priority
		{0x1700, "CW"},  // knx-demo object 4: 1 bit, write
		{0xFC00, "CRWTUI"},
		{0x0307, ""}, // priority and type only
	}

	for _, c := range cases {
		if got := DescriptorFlags(c.descriptor).String(); got != c.flags {
			t.Errorf("DescriptorFlags(%#04x) = %s, expected %s", c.descriptor, got, c.flags)
		}
	}
}

// loaded returns a loaded table with the given content.
func loaded(m *memory, data ...byte) table {
	s := m.alloc(len(data))
	copy(s.data, data)

	return table{state: stateLoaded, segment: s}
}

func TestBindings(t *testing.T) {
	e := &Device{}
	ga := func(main, middle, sub uint8) cemi.GroupAddr { return cemi.NewGroupAddr3(main, middle, sub) }

	e.tables[addressTable] = loaded(&e.memory,
		0, 4, // 4 addresses: 1/0/1, 1/0/2, 1/0/3, 1/0/4
		0x08, 0x01, 0x08, 0x02, 0x08, 0x03, 0x08, 0x04)
	e.tables[associationTable] = loaded(&e.memory,
		0, 5, // (TSAP, ASAP): sending addresses first
		0, 2, 0, 1,
		0, 3, 0, 2,
		0, 4, 0, 4,
		0, 1, 0, 1,
		0, 9, 0, 9) // out of range: ignored
	e.tables[groupObjectTable] = loaded(&e.memory,
		0, 4,
		0x4F, 0x07, 0x4F, 0x07, 0x4F, 0x07, 0x17, 0x00)

	expected := []device.Binding{
		{Flags: device.FlagCommunicate | device.FlagRead | device.FlagTransmit, Addresses: []cemi.GroupAddr{ga(1, 0, 2), ga(1, 0, 1)}},
		{Flags: device.FlagCommunicate | device.FlagRead | device.FlagTransmit, Addresses: []cemi.GroupAddr{ga(1, 0, 3)}},
		{Flags: device.FlagCommunicate | device.FlagRead | device.FlagTransmit},
		{Flags: device.FlagCommunicate | device.FlagWrite, Addresses: []cemi.GroupAddr{ga(1, 0, 4)}},
	}

	if got := e.bindings(); !reflect.DeepEqual(got, expected) {
		t.Errorf("Bindings\n%+v\nexpected\n%+v", got, expected)
	}

	// Truncated tables are read as far as they go.
	if w := words([]byte{0, 3, 0, 1, 0, 2, 0}, 1); !reflect.DeepEqual(w, []uint16{1, 2}) {
		t.Errorf("words = %v", w)
	}

	if w := words([]byte{0xFF, 0xFF}, 1); len(w) != 0 {
		t.Errorf("words = %v", w)
	}
}

func TestImage(t *testing.T) {
	e := &Device{progVersion: [5]byte{0x00, 0xFA, 0xAB, 0xCE, 0x79}}
	e.tables[addressTable] = loaded(&e.memory, 0, 1, 0x08, 0x01)
	e.tables[applicationProgram] = loaded(&e.memory, 1, 2, 3, 4, 5, 6, 7, 8)
	e.tables[groupObjectTable].state = stateLoading
	e.tables[associationTable] = table{state: stateError, errorCode: errInvalidOpcode}

	image := e.encode()

	restored := &Device{}
	if !restored.decode(image) {
		t.Fatal("decode failed")
	}

	if restored.progVersion != e.progVersion || !bytes.Equal(restored.encode(), image) {
		t.Errorf("Restored image differs:\n% x\n% x", restored.encode(), image)
	}

	if s := restored.tables[applicationProgram].segment; s == nil || s.address != e.tables[applicationProgram].segment.address {
		t.Errorf("Application segment %+v", s)
	}

	for _, bad := range [][]byte{nil, []byte("E\x02"), image[:len(image)-1], append(image, 0)} {
		if (&Device{}).decode(bad) {
			t.Errorf("Decoded a bad image % x", bad)
		}
	}
}
