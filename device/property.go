package device

import "github.com/shabunin/tiny-knx-go/apdu"

// A Property is a property of an interface object, read and written by ETS with
// PropertyValueRead and PropertyValueWrite.
//
// The functions are called with the device lock held: they must not call methods of the Device or
// its objects, but may use Later.
type Property struct {
	ID          uint8
	Type        uint8  // property datatype (PDT), e.g. 0x04 for unsigned int
	MaxElements uint16 // reported in the description
	ReadLevel   uint8  // access levels reported in the description, 0 (highest) to 15
	WriteLevel  uint8

	// Elements returns the current number of elements, read at index 0. Nil means 1.
	Elements func() uint16

	// Read returns count elements starting at start (1 or more). Nil data rejects the request.
	Read func(start uint16, count uint8) []byte

	// Write stores count elements starting at start (0 to reset the property, if it supports it).
	// It returns false to reject the request. Nil means the property is read-only.
	Write func(start uint16, count uint8, data []byte) bool
}

func (p *Property) elements() uint16 {
	if p.Elements == nil {
		return 1
	}

	return p.Elements()
}

// An InterfaceObject is a group of properties, e.g. the address table of a device programmed by
// ETS. The device adds PID_OBJECT_TYPE as the first property.
type InterfaceObject struct {
	Type       uint16
	Properties []Property
}

// Memory is device memory that ETS reads and writes with MemoryRead and MemoryWrite (16-bit
// addresses) or their extended forms (24-bit addresses). Its methods are called with the device
// lock held, like those of a Property, and return false if the range is not accessible.
type Memory interface {
	ReadMemory(address uint32, data []byte) bool
	WriteMemory(address uint32, data []byte) bool
}

// AddInterfaceObject adds an interface object and returns its index. Index 0 is the device
// object.
func (d *Device) AddInterfaceObject(obj InterfaceObject) uint8 {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.addInterfaceObject(obj)
}

func (d *Device) addInterfaceObject(obj InterfaceObject) uint8 {
	objectType := []byte{byte(obj.Type >> 8), byte(obj.Type)}
	properties := append([]Property{constant(pidObjectType, pdtUnsignedInt, objectType)}, obj.Properties...)

	d.interfaceObjects = append(d.interfaceObjects, InterfaceObject{Type: obj.Type, Properties: properties})

	return uint8(len(d.interfaceObjects) - 1)
}

// SetMemory sets the memory that MemoryRead and MemoryWrite access. Without memory, reads are
// rejected and writes ignored.
func (d *Device) SetMemory(m Memory) {
	d.mu.Lock()
	d.memory = m
	d.mu.Unlock()
}

// Later schedules f to run once the device lock is released. Call it only from Property and Memory
// functions, e.g. to call methods of the Device after a property changed.
func (d *Device) Later(f func()) {
	d.later(f)
}

// constant returns a read-only property with one element.
func constant(id, pdt uint8, value []byte) Property {
	return Property{ID: id, Type: pdt, MaxElements: 1, ReadLevel: 3, Read: func(uint16, uint8) []byte { return value }}
}

// property finds a property by object index and ID. The lock is held.
func (d *Device) property(object, id uint8) *Property {
	if int(object) >= len(d.interfaceObjects) || id == 0 {
		return nil
	}

	properties := d.interfaceObjects[object].Properties
	for i := range properties {
		if properties[i].ID == id {
			return &properties[i]
		}
	}

	return nil
}

func rejected(ref apdu.PropertyRef) apdu.PropertyValueResponse {
	ref.Count = 0
	return apdu.PropertyValueResponse{PropertyRef: ref}
}

func (d *Device) readProperty(ref apdu.PropertyRef) apdu.APDU {
	p := d.property(ref.Object, ref.PropertyID)
	if p == nil || ref.Count == 0 || p.Read == nil {
		return rejected(ref)
	}

	// Index 0 holds the number of elements.
	if ref.Start == 0 {
		n := p.elements()
		ref.Count = 1

		return apdu.PropertyValueResponse{PropertyRef: ref, Data: []byte{byte(n >> 8), byte(n)}}
	}

	if uint32(ref.Start)+uint32(ref.Count)-1 > uint32(p.elements()) {
		return rejected(ref)
	}

	data := p.Read(ref.Start, ref.Count)
	if data == nil {
		return rejected(ref)
	}

	return apdu.PropertyValueResponse{PropertyRef: ref, Data: append([]byte(nil), data...)}
}

// writeProperty writes a property and answers with its new value.
func (d *Device) writeProperty(ref apdu.PropertyRef, data []byte) apdu.APDU {
	p := d.property(ref.Object, ref.PropertyID)
	if p == nil || p.Write == nil || ref.Count == 0 {
		return rejected(ref)
	}

	if ref.Start != 0 && uint32(ref.Start)+uint32(ref.Count)-1 > uint32(p.MaxElements) {
		return rejected(ref)
	}

	if !p.Write(ref.Start, ref.Count, data) {
		return rejected(ref)
	}

	return d.readProperty(ref)
}

// describeProperty finds a property by ID or, with ID 0, by index. MaxElements 0 in the response
// means that there is no such property.
func (d *Device) describeProperty(request apdu.PropertyDescriptionRead) apdu.APDU {
	response := apdu.PropertyDescriptionResponse{
		Object:     request.Object,
		PropertyID: request.PropertyID,
		Index:      request.Index,
	}

	if int(request.Object) >= len(d.interfaceObjects) {
		return response
	}

	for i, p := range d.interfaceObjects[request.Object].Properties {
		if (request.PropertyID != 0 && p.ID == request.PropertyID) ||
			(request.PropertyID == 0 && i == int(request.Index)) {
			response.PropertyID = p.ID
			response.Index = uint8(i)
			response.Type = p.Type
			response.WriteEnable = p.Write != nil
			response.MaxElements = p.MaxElements
			response.ReadLevel = p.ReadLevel
			response.WriteLevel = p.WriteLevel

			return response
		}
	}

	return response
}
