// Package ets makes a device programmable by ETS as a System B device (mask version 0x07B0), with
// the interface objects of the thelsing KNX stack:
//
//	0 device object (package device)
//	1 group address table
//	2 group object association table
//	3 group object table
//	4 application program, whose segment holds the parameters
//
// ETS loads each table with the load state machine of its PID_LOAD_STATE_CONTROL: it unloads it,
// starts loading, allocates a relative segment, reads the segment's address from
// PID_TABLE_REFERENCE, writes it with MemoryWrite and completes loading. Once all four are loaded,
// the device is configured: the tables set the flags and group addresses of the device's group
// objects (device.Bind) and the application reads its parameters with Param.
//
// The group objects themselves (number, names, DPTs) are defined by the firmware, in the order of
// the ComObject numbers of the product database: object index i is ComObject number i+1.
package ets

import (
	"errors"
	"sync"

	"github.com/shabunin/tiny-knx-go/cemi"
	"github.com/shabunin/tiny-knx-go/device"
)

// Storage keeps the downloaded configuration across restarts, e.g. in flash or a file. The image
// is opaque.
type Storage interface {
	// Load returns the saved image, or nil if there is none.
	Load() ([]byte, error)
	Save(image []byte) error
}

// Interface object types.
const (
	objectTypeAddresses    = 1
	objectTypeAssociations = 2
	objectTypeApplication  = 3
	objectTypeGroupObjects = 9
)

// Positions in Device.tables; the interface object index is one more.
const (
	addressTable = iota
	associationTable
	groupObjectTable
	applicationProgram
)

// Property IDs of the table objects.
const (
	pidLoadStateControl = 5
	pidTableReference   = 7
	pidProgVersion      = 13
	pidPEIType          = 16
	pidMCBTable         = 27
	pidErrorCode        = 28
)

// Property datatypes (PDT).
const (
	pdtControl      = 0x00
	pdtUnsignedChar = 0x02
	pdtUnsignedLong = 0x09
	pdtGeneric05    = 0x15
	pdtGeneric08    = 0x18
	pdtEnum8        = 0x35
)

// ErrBadImage is returned by Attach if the stored image cannot be decoded.
var ErrBadImage = errors.New("ets: invalid stored configuration")

// A Device is the ETS side of a device: its tables, parameters and program version.
type Device struct {
	dev     *device.Device
	storage Storage

	// mu guards the fields below. It is taken with the device lock held (in Property and Memory
	// functions), so it must never be held while calling a method of dev.
	mu           sync.Mutex
	memory       memory
	tables       [4]table
	progVersion  [5]byte
	configured   bool
	onConfigured func()
}

// Attach adds the System B interface objects and memory to d and applies the configuration saved
// in storage, if any. Call it once, after device.New and before Run; d must not have other
// interface objects yet. storage may be nil.
//
// Until ETS starts a download, the group objects keep the configuration that d was created with.
// While ETS loads, they do not communicate.
func Attach(d *device.Device, storage Storage) (*Device, error) {
	e := &Device{dev: d, storage: storage}

	types := [4]uint16{objectTypeAddresses, objectTypeAssociations, objectTypeGroupObjects, objectTypeApplication}
	for i := range e.tables {
		e.tables[i].objectType = types[i]
	}

	if storage != nil {
		image, err := storage.Load()
		if err != nil {
			return nil, err
		}

		if len(image) > 0 && !e.decode(image) {
			return nil, ErrBadImage
		}
	}

	for i := range e.tables {
		if index := d.AddInterfaceObject(e.interfaceObject(i)); int(index) != i+1 {
			return nil, errors.New("ets: the device already has interface objects")
		}
	}

	d.SetMemory(memoryAccess{e})

	if e.isConfigured() {
		e.configured = true
		d.Bind(e.bindings())
	}

	return e, nil
}

// Configured reports whether ETS has loaded all tables.
func (e *Device) Configured() bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	return e.configured
}

// OnConfigured registers a function that is called when a download completes, e.g. to read the
// parameters again. It runs on the device's goroutine.
func (e *Device) OnConfigured(f func()) {
	e.mu.Lock()
	e.onConfigured = f
	e.mu.Unlock()
}

// ProgramVersion returns PID_PROG_VERSION as ETS wrote it: manufacturer ID (2 bytes), application
// number (2 bytes) and application version.
func (e *Device) ProgramVersion() [5]byte {
	e.mu.Lock()
	defer e.mu.Unlock()

	return e.progVersion
}

// Param returns the parameter byte at offset in the application segment ("Parameters" in the
// product database), or 0 if the application program is not loaded.
func (e *Device) Param(offset int) byte {
	if b := e.ParamBytes(offset, 1); len(b) == 1 {
		return b[0]
	}

	return 0
}

// ParamBytes returns a copy of n parameter bytes at offset. Bytes outside the loaded application
// segment are 0. Parameters are big-endian.
func (e *Device) ParamBytes(offset, n int) []byte {
	e.mu.Lock()
	defer e.mu.Unlock()

	params := make([]byte, n)

	if data := e.tables[applicationProgram].data(); offset >= 0 && offset < len(data) {
		copy(params, data[offset:])
	}

	return params
}

// isConfigured reports whether all tables are loaded. e.mu is held.
func (e *Device) isConfigured() bool {
	for i := range e.tables {
		if e.tables[i].state != stateLoaded {
			return false
		}
	}

	return true
}

// update applies the tables to the device and saves them after ETS changed a load state. It runs
// without the device lock.
func (e *Device) update() {
	e.mu.Lock()
	image := e.encode()
	configured := e.isConfigured()
	completed := configured && !e.configured
	e.configured = configured

	var bindings []device.Binding
	if configured {
		bindings = e.bindings()
	}

	onConfigured := e.onConfigured
	e.mu.Unlock()

	if e.storage != nil {
		e.storage.Save(image)
	}

	e.dev.Bind(bindings)

	if completed && onConfigured != nil {
		onConfigured()
	}
}

// save saves the tables. It runs without the device lock.
func (e *Device) save() {
	if e.storage == nil {
		return
	}

	e.mu.Lock()
	image := e.encode()
	e.mu.Unlock()

	e.storage.Save(image)
}

// interfaceObject returns the interface object of a table. Its functions run with the device lock
// held.
func (e *Device) interfaceObject(i int) device.InterfaceObject {
	t := &e.tables[i]

	locked := func(f func() []byte) func(uint16, uint8) []byte {
		return func(uint16, uint8) []byte {
			e.mu.Lock()
			defer e.mu.Unlock()

			return f()
		}
	}

	var properties []device.Property

	if i == applicationProgram {
		properties = append(properties,
			device.Property{
				ID: pidProgVersion, Type: pdtGeneric05, MaxElements: 1, ReadLevel: 3, WriteLevel: 3,
				Read: locked(func() []byte { return append([]byte(nil), e.progVersion[:]...) }),
				Write: func(start uint16, count uint8, data []byte) bool {
					if start != 1 || count != 1 || len(data) < len(e.progVersion) {
						return false
					}

					e.mu.Lock()
					copy(e.progVersion[:], data)
					e.mu.Unlock()

					e.dev.Later(e.save)

					return true
				},
			},
			device.Property{
				ID: pidPEIType, Type: pdtUnsignedChar, MaxElements: 1, ReadLevel: 3,
				Read: func(uint16, uint8) []byte { return []byte{0} },
			})
	}

	return device.InterfaceObject{Type: t.objectType, Properties: append(properties,
		device.Property{
			ID: pidLoadStateControl, Type: pdtControl, MaxElements: 1, ReadLevel: 3, WriteLevel: 3,
			Read: locked(func() []byte { return []byte{t.state} }),
			Write: func(start uint16, count uint8, data []byte) bool {
				if start != 1 || count != 1 || len(data) < 1 {
					return false
				}

				e.mu.Lock()
				t.loadEvent(&e.memory, data)
				e.mu.Unlock()

				e.dev.Later(e.update)

				return true
			},
		},
		device.Property{
			ID: pidTableReference, Type: pdtUnsignedLong, MaxElements: 1, ReadLevel: 3,
			Read: locked(func() []byte {
				ref := t.reference()
				return []byte{byte(ref >> 24), byte(ref >> 16), byte(ref >> 8), byte(ref)}
			}),
		},
		device.Property{
			ID: pidMCBTable, Type: pdtGeneric08, MaxElements: 1, ReadLevel: 3,
			Read: locked(t.mcb),
		},
		device.Property{
			ID: pidErrorCode, Type: pdtEnum8, MaxElements: 1, ReadLevel: 3,
			Read: locked(func() []byte { return []byte{t.errorCode} }),
		},
	)}
}

// memoryAccess gives ETS access to the segments.
type memoryAccess struct{ e *Device }

func (m memoryAccess) ReadMemory(address uint32, data []byte) bool {
	m.e.mu.Lock()
	defer m.e.mu.Unlock()

	return copy(data, m.e.memory.find(address, len(data))) == len(data)
}

func (m memoryAccess) WriteMemory(address uint32, data []byte) bool {
	m.e.mu.Lock()
	defer m.e.mu.Unlock()

	return copy(m.e.memory.find(address, len(data)), data) == len(data)
}

// bindings converts the loaded tables into flags and group addresses of the group objects. e.mu is
// held.
func (e *Device) bindings() []device.Binding {
	addresses := words(e.tables[addressTable].data(), 1)
	associations := words(e.tables[associationTable].data(), 2)
	descriptors := words(e.tables[groupObjectTable].data(), 1)

	bindings := make([]device.Binding, len(descriptors))
	for i, descriptor := range descriptors {
		bindings[i].Flags = DescriptorFlags(descriptor)
	}

	// The association table lists (TSAP, ASAP) pairs: TSAP n is the n-th group address, ASAP n
	// the n-th group object. ETS puts the sending address of each object first.
	for i := 0; i+1 < len(associations); i += 2 {
		tsap, asap := int(associations[i]), int(associations[i+1])

		if tsap >= 1 && tsap <= len(addresses) && asap >= 1 && asap <= len(bindings) {
			b := &bindings[asap-1]
			b.Addresses = append(b.Addresses, cemi.GroupAddr(addresses[tsap-1]))
		}
	}

	return bindings
}

// words parses a table: a 16-bit entry count, followed by entries of size 16-bit words each.
func words(data []byte, size int) []uint16 {
	if len(data) < 2 {
		return nil
	}

	n := int(data[0])<<8 | int(data[1])
	if max := (len(data) - 2) / (2 * size); n > max {
		n = max
	}

	values := make([]uint16, n*size)
	for i := range values {
		values[i] = uint16(data[2+2*i])<<8 | uint16(data[3+2*i])
	}

	return values
}

// DescriptorFlags converts a group object descriptor of the System B group object table into
// flags. The high byte holds the flags, the low byte the value type:
//
//	bit 15 U, 14 T, 13 I, 12 W, 11 R, 10 C, 9–8 priority
func DescriptorFlags(descriptor uint16) device.Flags {
	var flags device.Flags

	bits := []struct {
		bit  uint
		flag device.Flags
	}{
		{10, device.FlagCommunicate},
		{11, device.FlagRead},
		{12, device.FlagWrite},
		{13, device.FlagInit},
		{14, device.FlagTransmit},
		{15, device.FlagUpdate},
	}

	for _, b := range bits {
		if descriptor&(1<<b.bit) != 0 {
			flags |= b.flag
		}
	}

	return flags
}
