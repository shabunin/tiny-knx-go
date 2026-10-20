package ets

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/shabunin/tiny-knx-go/apdu"
	"github.com/shabunin/tiny-knx-go/cemi"
	"github.com/shabunin/tiny-knx-go/device"
	"github.com/shabunin/tiny-knx-go/dpt"
	"github.com/shabunin/tiny-knx-go/link/mock"
	"github.com/shabunin/tiny-knx-go/transport"
)

// This file replays an ETS download of the thelsing knx-demo product (knx-demo-tp.knxprod: M-00FA,
// application 0xABCE version 0x79, mask 0x07B0) into a device with the same group objects.
//
// Taken from the product database: the parameter segment (8 bytes, LsmIdx 4), its LdCtrlRelSegment
// and LdCtrlWriteRelMem with Verify, the program version and the four ComObjects with their
// flags.
//
// Assumptions about ETS, not verified against a real download:
//   - the order of the steps of the System B load procedure (unload all, then load the address,
//     association and group object tables, then the application program, then restart);
//   - every PID_LOAD_STATE_CONTROL write carries 10 bytes; LdCtrlRelSegment is the additional load
//     control 03 0B <size:4> <mode> <fill> 00 00, as the thelsing stack decodes it;
//   - ETS reads the segment address from PID_TABLE_REFERENCE and writes 12 bytes per MemoryWrite
//     (MAX_APDU_LENGTH 15), with PID_DEVICE_CONTROL verify mode on for the tables;
//   - the table formats of the thelsing stack: a 16-bit count, then group addresses, (TSAP, ASAP)
//     pairs with the sending addresses first, and group object descriptors;
//   - ETS reads the properties checked below before it downloads.

var (
	deviceAddr = cemi.NewIndividualAddr3(1, 1, 10)
	toolAddr   = cemi.NewIndividualAddr3(1, 1, 240)
	senderAddr = cemi.NewIndividualAddr3(1, 1, 1)

	gaCurrent  = cemi.NewGroupAddr3(1, 0, 1)
	gaMax      = cemi.NewGroupAddr3(1, 0, 2)
	gaMin      = cemi.NewGroupAddr3(1, 0, 3)
	gaReset    = cemi.NewGroupAddr3(1, 0, 4)
	gaResetAll = cemi.NewGroupAddr3(1, 0, 5)

	params      = []byte{5, 1, 2, 1, 0xFF, 0xFF, 0xFF, 0xF6} // startup 5 s, …, Abgleich -10
	progVersion = []byte{0x00, 0xFA, 0xAB, 0xCE, 0x79}
)

// The tables that ETS computes for the download.
var (
	addressTableData = []byte{
		0, 5,
		0x08, 0x01, 0x08, 0x02, 0x08, 0x03, 0x08, 0x04, 0x08, 0x05,
	}
	associationTableData = []byte{
		0, 5,
		0, 1, 0, 1,
		0, 2, 0, 2,
		0, 3, 0, 3,
		0, 4, 0, 4,
		0, 5, 0, 4, // a second address for the reset object
	}
	groupObjectTableData = []byte{
		0, 4,
		0x4F, 0x07, // C R T, low priority, 2 bytes
		0x4F, 0x07,
		0x4F, 0x07,
		0x17, 0x00, // C W, low priority, 1 bit
	}
)

type memStorage struct {
	mu    sync.Mutex
	image []byte
	saves int
}

func (s *memStorage) Load() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]byte(nil), s.image...), nil
}

func (s *memStorage) Save(image []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.image = append([]byte(nil), image...)
	s.saves++

	return nil
}

// knxDemo creates a device with the group objects of knx-demo. The firmware links nothing.
func knxDemo(t *testing.T) *device.Device {
	t.Helper()

	d, err := device.New(device.Identity{
		SerialNumber: apdu.SerialNumber{0x00, 0xFA, 0x01, 0x24, 0x00, 0x01},
		OrderNumber:  "IA4313",
	}, device.Config{
		Address: deviceAddr,
		Objects: []device.ObjectConfig{
			{Name: "current", DPT: "9.001"},
			{Name: "max", DPT: "9.001"},
			{Name: "min", DPT: "9.001"},
			{Name: "reset", DPT: "1.017"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	return d
}

func run(t *testing.T, bus *mock.Bus, d *device.Device) {
	t.Helper()

	l := bus.Attach(deviceAddr)
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

// tool performs the steps of an ETS download over a connection.
type tool struct {
	t      *testing.T
	client *transport.Client
}

func (tl *tool) readProperty(object, pid uint8) []byte {
	tl.t.Helper()

	value, err := tl.client.ReadProperty(object, pid, 1, 1)
	if err != nil {
		tl.t.Fatalf("Reading property %d/%d: %v", object, pid, err)
	}

	return value
}

func (tl *tool) writeProperty(object, pid uint8, data []byte) []byte {
	tl.t.Helper()

	write := apdu.PropertyValueWrite{
		PropertyRef: apdu.PropertyRef{Object: object, PropertyID: pid, Count: 1, Start: 1},
		Data:        data,
	}

	response, err := tl.client.Request(write, func(a apdu.APDU) bool {
		r, ok := a.(apdu.PropertyValueResponse)
		return ok && r.Object == object && r.PropertyID == pid
	})
	if err != nil {
		tl.t.Fatalf("Writing property %d/%d: %v", object, pid, err)
	}

	r := response.(apdu.PropertyValueResponse)
	if r.Count == 0 {
		tl.t.Fatalf("Writing property %d/%d was rejected", object, pid)
	}

	return r.Data
}

// loadEvent writes PID_LOAD_STATE_CONTROL and checks the resulting state.
func (tl *tool) loadEvent(object uint8, event []byte, state uint8) {
	tl.t.Helper()

	if got := tl.writeProperty(object, pidLoadStateControl, event); len(got) != 1 || got[0] != state {
		tl.t.Fatalf("Object %d: event % x led to state % x, expected %d", object, event[:2], got, state)
	}
}

// writeMemory writes data in 12-byte chunks and verifies it.
func (tl *tool) writeMemory(address uint16, data []byte, verifyMode bool) {
	tl.t.Helper()

	for offset := 0; offset < len(data); offset += 12 {
		chunk := data[offset:min(offset+12, len(data))]
		addr := address + uint16(offset)
		write := apdu.MemoryWrite{Address: addr, Data: chunk}

		if verifyMode {
			// The device answers with the written memory.
			response, err := tl.client.Request(write, func(a apdu.APDU) bool {
				r, ok := a.(apdu.MemoryResponse)
				return ok && r.Address == addr
			})
			if err != nil || !bytes.Equal(response.(apdu.MemoryResponse).Data, chunk) {
				tl.t.Fatalf("MemoryWrite at %#04x: %+v, %v", addr, response, err)
			}
		} else if _, err := tl.client.Request(write, nil); err != nil {
			tl.t.Fatalf("MemoryWrite at %#04x: %v", addr, err)
		}

		if got, err := tl.client.ReadMemory(addr, uint8(len(chunk))); err != nil || !bytes.Equal(got, chunk) {
			tl.t.Fatalf("Verifying %#04x: % x, %v", addr, got, err)
		}
	}
}

// load loads one table: start loading, allocate a relative segment, write it, complete.
func (tl *tool) load(object uint8, data []byte, verifyMode bool) {
	tl.t.Helper()

	tl.loadEvent(object, lsc(eventStartLoading), stateLoading)

	size := len(data)
	tl.loadEvent(object, lsc(eventAdditionalLoadControls, controlRelativeAllocation,
		byte(size>>24), byte(size>>16), byte(size>>8), byte(size), 0, 0), stateLoading)

	ref := tl.readProperty(object, pidTableReference)
	if len(ref) != 4 || ref[0] != 0 || ref[1] != 0 || ref[2] == 0 {
		tl.t.Fatalf("Object %d: table reference % x", object, ref)
	}

	tl.writeMemory(uint16(ref[2])<<8|uint16(ref[3]), data, verifyMode)

	if object == applicationProgram+1 {
		if got := tl.writeProperty(object, pidProgVersion, progVersion); !bytes.Equal(got, progVersion) {
			tl.t.Fatalf("PID_PROG_VERSION = % x", got)
		}
	}

	tl.loadEvent(object, lsc(eventLoadCompleted), stateLoaded)

	if mcb := tl.readProperty(object, pidMCBTable); len(mcb) != 8 || int(mcb[3]) != size {
		tl.t.Errorf("Object %d: MCB % x", object, mcb)
	}
}

func TestDownload(t *testing.T) {
	bus := mock.NewBus()
	storage := &memStorage{}

	d := knxDemo(t)

	e, err := Attach(d, storage)
	if err != nil {
		t.Fatal(err)
	}

	configured := make(chan struct{}, 1)
	e.OnConfigured(func() { configured <- struct{}{} })

	run(t, bus, d)

	client := transport.NewClient(bus.Attach(toolAddr), transport.DefaultConfig)
	defer client.Close()

	tl := &tool{t: t, client: client}

	if err := client.Connect(deviceAddr); err != nil {
		t.Fatal(err)
	}

	if mask, err := client.DeviceDescriptor(); err != nil || mask != 0x07B0 {
		t.Fatalf("Mask %#04x, %v", mask, err)
	}

	authorize, err := client.Request(apdu.AuthorizeRequest{Key: 0xFFFFFFFF}, func(a apdu.APDU) bool {
		_, ok := a.(apdu.AuthorizeResponse)
		return ok
	})
	if err != nil || authorize.(apdu.AuthorizeResponse).Level != 0 {
		t.Fatalf("Authorize: %+v, %v", authorize, err)
	}

	// What ETS checks before it downloads.
	checks := []struct {
		object, pid uint8
		value       []byte
	}{
		{0, 12, []byte{0x00, 0xFA}},            // manufacturer ID
		{0, 56, []byte{0, 15}},                 // MAX_APDU_LENGTH
		{0, 83, []byte{0x07, 0xB0}},            // DEVICE_DESCRIPTOR
		{0, 78, []byte{0, 0, 0, 0, 0, 0}},      // HARDWARE_TYPE
		{4, pidProgVersion, make([]byte, 5)},   // nothing loaded yet
		{1, 1, []byte{0, objectTypeAddresses}}, // OBJECT_TYPE of each table
		{2, 1, []byte{0, objectTypeAssociations}},
		{3, 1, []byte{0, objectTypeGroupObjects}},
		{4, 1, []byte{0, objectTypeApplication}},
		{4, pidPEIType, []byte{0}},
		{1, pidLoadStateControl, []byte{stateUnloaded}},
		{1, pidTableReference, []byte{0, 0, 0, 0}},
	}

	for _, c := range checks {
		if got := tl.readProperty(c.object, c.pid); !bytes.Equal(got, c.value) {
			t.Errorf("Property %d/%d = % x, expected % x", c.object, c.pid, got, c.value)
		}
	}

	if count, err := client.ReadProperty(0, 71, 0, 1); err != nil || !bytes.Equal(count, []byte{0, 5}) {
		t.Errorf("IO_LIST has % x elements, %v", count, err)
	}

	if list, err := client.ReadProperty(0, 71, 1, 5); err != nil || !bytes.Equal(list, []byte{0, 0, 0, 1, 0, 2, 0, 9, 0, 3}) {
		t.Errorf("IO_LIST = % x, %v", list, err)
	}

	// ETS learns the format of the association table from the description of its PID_TABLE.
	description, err := client.Request(apdu.PropertyDescriptionRead{Object: 2, PropertyID: pidTable}, func(a apdu.APDU) bool {
		_, ok := a.(apdu.PropertyDescriptionResponse)
		return ok
	})
	if r, _ := description.(apdu.PropertyDescriptionResponse); err != nil || r.Type != pdtGeneric04 || r.MaxElements == 0 {
		t.Fatalf("PID_TABLE description %+v, %v", description, err)
	}

	// Verify mode: MemoryWrite is answered with the written data.
	if got := tl.writeProperty(0, 14, []byte{0x04}); !bytes.Equal(got, []byte{0x04}) {
		t.Fatalf("DEVICE_CONTROL = % x", got)
	}

	for object := uint8(1); object <= 4; object++ {
		tl.loadEvent(object, lsc(eventUnload), stateUnloaded)
	}

	tl.load(1, addressTableData, true)
	tl.load(2, associationTableData, true)
	tl.load(3, groupObjectTableData, true)

	if count, err := client.ReadProperty(2, pidTable, 0, 1); err != nil || !bytes.Equal(count, associationTableData[:2]) {
		t.Errorf("PID_TABLE has % x elements, %v", count, err)
	}

	if entries, err := client.ReadProperty(2, pidTable, 2, 2); err != nil || !bytes.Equal(entries, associationTableData[6:14]) {
		t.Errorf("PID_TABLE[2:3] = % x, %v", entries, err)
	}

	if e.Configured() {
		t.Error("Configured before the application program was loaded")
	}

	tl.writeProperty(0, 14, []byte{0x00})
	tl.load(4, params, false)

	select {
	case <-configured:
	case <-time.After(time.Second):
		t.Fatal("OnConfigured not called")
	}

	if _, err := client.Request(apdu.Restart{}, nil); err != nil {
		t.Fatalf("Restart: %v", err)
	}

	// The device is configured.
	if !e.Configured() || e.ProgramVersion() != [5]byte(progVersion) {
		t.Errorf("Configured %v, program version % x", e.Configured(), e.ProgramVersion())
	}

	if got := e.ParamBytes(0, 8); !bytes.Equal(got, params) || e.Param(0) != 5 || e.Param(8) != 0 {
		t.Errorf("Parameters % x", got)
	}

	checkConfig(t, d)
	checkGroupCommunication(t, bus, d)

	// After a restart of the firmware, the configuration comes from storage.
	d2 := knxDemo(t)

	e2, err := Attach(d2, storage)
	if err != nil {
		t.Fatal(err)
	}

	if !e2.Configured() || e2.ProgramVersion() != [5]byte(progVersion) || !bytes.Equal(e2.ParamBytes(0, 8), params) {
		t.Errorf("Restored: configured %v, program version % x, parameters % x", e2.Configured(), e2.ProgramVersion(), e2.ParamBytes(0, 8))
	}

	checkConfig(t, d2)
}

func checkConfig(t *testing.T, d *device.Device) {
	t.Helper()

	expected := []struct {
		flags     string
		addresses []cemi.GroupAddr
	}{
		{"CRT", []cemi.GroupAddr{gaCurrent}},
		{"CRT", []cemi.GroupAddr{gaMax}},
		{"CRT", []cemi.GroupAddr{gaMin}},
		{"CW", []cemi.GroupAddr{gaReset, gaResetAll}},
	}

	config := d.Config()
	if config.Address != deviceAddr || len(config.Objects) != len(expected) {
		t.Fatalf("Config %+v", config)
	}

	for i, oc := range config.Objects {
		if oc.Flags.String() != expected[i].flags || !equalAddrs(oc.Addresses, expected[i].addresses) {
			t.Errorf("Object %d (%s): %s %v, expected %s %v", i, oc.Name, oc.Flags, oc.Addresses, expected[i].flags, expected[i].addresses)
		}
	}
}

func equalAddrs(a, b []cemi.GroupAddr) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

// checkGroupCommunication checks that group telegrams reach the downloaded objects.
func checkGroupCommunication(t *testing.T, bus *mock.Bus, d *device.Device) {
	t.Helper()

	sender := bus.Attach(senderAddr)
	defer sender.Close()

	send := func(ga cemi.GroupAddr, a apdu.APDU) {
		t.Helper()

		err := sender.Send(&cemi.LDataReq{LData: cemi.LData{
			Control1:    cemi.Control1Request(cemi.PrioLow),
			Control2:    cemi.Control2GroupAddr | cemi.Control2Hops(6),
			Destination: uint16(ga),
			Data:        apdu.Encode(a),
		}})
		if err != nil {
			t.Fatal(err)
		}
	}

	receive := func() (*cemi.LDataInd, apdu.APDU) {
		t.Helper()

		for {
			select {
			case ind := <-sender.Inbound():
				if !ind.Control2.IsGroupAddr() {
					continue // the tool's connection
				}

				a, err := apdu.Decode(ind.Data.(*cemi.AppData))
				if err != nil {
					t.Fatal(err)
				}

				return ind, a

			case <-time.After(time.Second):
				t.Fatal("No group telegram")
				return nil, nil
			}
		}
	}

	reset := make(chan []byte, 2)
	d.Objects()[3].OnChange(func(o *device.Object) { reset <- o.Value() })

	// The reset object accepts writes on both addresses.
	for _, ga := range []cemi.GroupAddr{gaReset, gaResetAll} {
		send(ga, apdu.GroupValueWrite{Data: []byte{1}})

		select {
		case v := <-reset:
			if !bytes.Equal(v, []byte{1}) {
				t.Errorf("Reset object received % x", v)
			}
		case <-time.After(time.Second):
			t.Fatalf("Write to %v did not reach the reset object", ga)
		}
	}

	// The temperature transmits on its address and answers reads.
	d.Objects()[0].Set(dpt.DPT_9001(21.5))

	if ind, a := receive(); cemi.GroupAddr(ind.Destination) != gaCurrent {
		t.Errorf("Temperature sent to %v: %+v", cemi.GroupAddr(ind.Destination), a)
	} else if _, ok := a.(apdu.GroupValueWrite); !ok {
		t.Errorf("Temperature sent as %+v", a)
	}

	send(gaMax, apdu.GroupValueRead{})

	if ind, a := receive(); cemi.GroupAddr(ind.Destination) != gaMax {
		t.Errorf("Read of max answered on %v", cemi.GroupAddr(ind.Destination))
	} else if _, ok := a.(apdu.GroupValueResponse); !ok {
		t.Errorf("Read of max answered with %+v", a)
	}

	// The temperature has no W flag.
	before := d.Objects()[0].Value()
	send(gaCurrent, apdu.GroupValueWrite{Data: []byte{0, 0}})
	send(gaReset, apdu.GroupValueWrite{Data: []byte{0}}) // a marker that is processed after it

	<-reset

	if got := d.Objects()[0].Value(); !bytes.Equal(got, before) {
		t.Errorf("Temperature changed to % x by a write", got)
	}
}

// A partial download of the parameters leaves the tables and replaces the parameters.
func TestPartialDownload(t *testing.T) {
	bus := mock.NewBus()
	storage := &memStorage{}

	d := knxDemo(t)

	e, err := Attach(d, storage)
	if err != nil {
		t.Fatal(err)
	}

	run(t, bus, d)

	client := transport.NewClient(bus.Attach(toolAddr), transport.DefaultConfig)
	defer client.Close()

	if err := client.Connect(deviceAddr); err != nil {
		t.Fatal(err)
	}

	tl := &tool{t: t, client: client}

	for object := uint8(1); object <= 4; object++ {
		tl.loadEvent(object, lsc(eventUnload), stateUnloaded)
	}

	tl.load(1, addressTableData, false)
	tl.load(2, associationTableData, false)
	tl.load(3, groupObjectTableData, false)
	tl.load(4, params, false)

	// While the application is unloaded, the device is not configured and objects are silent.
	ref := tl.readProperty(4, pidTableReference)
	tl.loadEvent(4, lsc(eventUnload), stateUnloaded)

	eventually(t, "unconfigured", func() bool { return !e.Configured() && d.Objects()[0].Flags() == 0 })

	if e.Param(0) != 0 {
		t.Errorf("Parameter of an unloaded application: %d", e.Param(0))
	}

	// Like ETS, write the changed parameters at the old reference, without allocating.
	newParams := []byte{9, 9, 9, 9, 0, 0, 0, 1}

	tl.loadEvent(4, lsc(eventStartLoading), stateLoading)

	if again := tl.readProperty(4, pidTableReference); !bytes.Equal(again, ref) {
		t.Fatalf("Reference of the application % x after unloading, % x before", again, ref)
	}

	tl.writeMemory(uint16(ref[2])<<8|uint16(ref[3]), newParams, false)
	tl.loadEvent(4, lsc(eventLoadCompleted), stateLoaded)

	eventually(t, "configured", func() bool { return e.Configured() && d.Objects()[0].Flags() != 0 })

	if got := e.ParamBytes(0, 8); !bytes.Equal(got, newParams) {
		t.Errorf("Parameters % x", got)
	}

	checkConfig(t, d)
}

func TestAttach(t *testing.T) {
	d := knxDemo(t)
	d.AddInterfaceObject(device.InterfaceObject{Type: 99})

	if _, err := Attach(d, nil); err == nil {
		t.Error("Attached to a device with other interface objects")
	}

	if _, err := Attach(knxDemo(t), &memStorage{image: []byte("junk")}); err != ErrBadImage {
		t.Errorf("Bad image: %v", err)
	}

	// Without a stored configuration, the firmware's configuration stays.
	d, err := device.New(device.Identity{}, device.Config{Objects: []device.ObjectConfig{
		{Name: "switch", DPT: "1.001", Flags: device.FlagCommunicate | device.FlagWrite, Addresses: []cemi.GroupAddr{gaReset}},
	}})
	if err != nil {
		t.Fatal(err)
	}

	e, err := Attach(d, &memStorage{})
	if err != nil {
		t.Fatal(err)
	}

	if e.Configured() || d.Objects()[0].Flags() != device.FlagCommunicate|device.FlagWrite {
		t.Errorf("Firmware configuration lost: %v", d.Config())
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
