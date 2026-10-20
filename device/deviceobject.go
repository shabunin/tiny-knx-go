package device

// Property IDs of the device object (interface object 0).
const (
	pidObjectType       = 1
	pidFirmwareRevision = 9
	pidSerialNumber     = 11
	pidManufacturerID   = 12
	pidDeviceControl    = 14
	pidOrderInfo        = 15
	pidVersion          = 25
	pidRoutingCount     = 51
	pidProgMode         = 54
	pidMaxAPDULength    = 56
	pidSubnetAddr       = 57
	pidDeviceAddr       = 58
	pidIOList           = 71
	pidHardwareType     = 78
	pidDeviceDescriptor = 83
)

// Property datatypes (PDT).
const (
	pdtUnsignedChar = 0x02
	pdtUnsignedInt  = 0x04
	pdtGeneric02    = 0x12
	pdtGeneric06    = 0x16
	pdtGeneric10    = 0x1A
	pdtVersion      = 0x30
	pdtBitset8      = 0x33
)

// Bits of PID_DEVICE_CONTROL.
const (
	deviceControlUserStopped = 0x01
	deviceControlAddrDupl    = 0x02
	deviceControlVerify      = 0x04 // answer MemoryWrite with MemoryResponse
	deviceControlSafeState   = 0x08
)

// maxInterfaceObjects limits PID_IO_LIST.
const maxInterfaceObjects = 16

// deviceObject returns the device object, with the properties of a System B device in the order
// of the thelsing KNX stack. The values are read with the lock held.
func (d *Device) deviceObject() InterfaceObject {
	ident := &d.identity

	order := make([]byte, 10)
	copy(order, ident.OrderNumber)

	readWrite := func(id, pdt uint8, value *byte, mask byte) Property {
		return Property{
			ID: id, Type: pdt, MaxElements: 1, ReadLevel: 3, WriteLevel: 3,
			Read: func(uint16, uint8) []byte { return []byte{*value} },
			Write: func(start uint16, count uint8, data []byte) bool {
				if start != 1 || count != 1 || len(data) < 1 {
					return false
				}

				*value = data[0] & mask

				return true
			},
		}
	}

	progMode := Property{
		ID: pidProgMode, Type: pdtBitset8, MaxElements: 1, ReadLevel: 3, WriteLevel: 3,
		Read: func(uint16, uint8) []byte {
			if d.progMode {
				return []byte{1}
			}

			return []byte{0}
		},
		Write: func(start uint16, count uint8, data []byte) bool {
			if start != 1 || count != 1 || len(data) < 1 {
				return false
			}

			d.setProgMode(data[0]&1 == 1)

			return true
		},
	}

	return InterfaceObject{Type: 0, Properties: []Property{
		constant(pidFirmwareRevision, pdtUnsignedChar, []byte{ident.FirmwareRevision}),
		constant(pidSerialNumber, pdtGeneric06, ident.SerialNumber[:]),
		constant(pidManufacturerID, pdtUnsignedInt, ident.SerialNumber[:2]),
		readWrite(pidDeviceControl, pdtBitset8, &d.deviceControl, 0x0F),
		constant(pidOrderInfo, pdtGeneric10, order),
		constant(pidVersion, pdtVersion, []byte{0, ident.FirmwareRevision}),
		readWrite(pidRoutingCount, pdtUnsignedChar, &d.routingCount, 0x70),
		progMode,
		constant(pidMaxAPDULength, pdtUnsignedInt, []byte{0, 15}), // standard frames
		{ID: pidSubnetAddr, Type: pdtUnsignedChar, MaxElements: 1, ReadLevel: 3,
			Read: func(uint16, uint8) []byte { return []byte{byte(d.address >> 8)} }},
		{ID: pidDeviceAddr, Type: pdtUnsignedChar, MaxElements: 1, ReadLevel: 3,
			Read: func(uint16, uint8) []byte { return []byte{byte(d.address)} }},
		{ID: pidIOList, Type: pdtUnsignedInt, MaxElements: maxInterfaceObjects, ReadLevel: 3,
			Elements: func() uint16 { return uint16(len(d.interfaceObjects)) },
			Read: func(start uint16, count uint8) []byte {
				var types []byte
				for _, obj := range d.interfaceObjects[start-1 : int(start-1)+int(count)] {
					types = append(types, byte(obj.Type>>8), byte(obj.Type))
				}

				return types
			}},
		constant(pidHardwareType, pdtGeneric06, ident.HardwareType[:]),
		constant(pidDeviceDescriptor, pdtGeneric02, []byte{byte(ident.MaskVersion >> 8), byte(ident.MaskVersion)}),
	}}
}
