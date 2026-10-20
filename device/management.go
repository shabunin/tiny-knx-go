package device

import "github.com/shabunin/tiny-knx-go/apdu"

// handleBroadcast answers system-wide management requests: finding devices in programming mode
// and assigning individual addresses. The lock is held.
func (d *Device) handleBroadcast(request apdu.APDU) {
	switch request := request.(type) {
	case apdu.IndividualAddressRead:
		if d.progMode {
			d.sendBroadcast(apdu.IndividualAddressResponse{})
		}

	case apdu.IndividualAddressWrite:
		if d.progMode {
			d.setAddress(request.Address)
		}

	case apdu.IndividualAddressSerialNumberRead:
		if request.SerialNumber == d.identity.SerialNumber {
			d.sendBroadcast(apdu.IndividualAddressSerialNumberResponse{SerialNumber: d.identity.SerialNumber})
		}

	case apdu.IndividualAddressSerialNumberWrite:
		if request.SerialNumber == d.identity.SerialNumber {
			d.setAddress(request.Address)
		}
	}
}

// handleManagement answers a point-to-point request, received over a connection or
// connectionless. It returns the response, if any. The lock is held.
func (d *Device) handleManagement(request apdu.APDU, connected bool) apdu.APDU {
	switch request := request.(type) {
	case apdu.DeviceDescriptorRead:
		if request.Type != 0 {
			return apdu.DeviceDescriptorResponse{Type: 0x3F} // not supported
		}

		mask := d.identity.MaskVersion

		return apdu.DeviceDescriptorResponse{Descriptor: []byte{byte(mask >> 8), byte(mask)}}

	case apdu.PropertyValueRead:
		return d.readProperty(request.PropertyRef)

	case apdu.PropertyValueWrite:
		return d.writeProperty(request.PropertyRef, request.Data)

	case apdu.PropertyDescriptionRead:
		return d.describeProperty(request)

	case apdu.Restart:
		if !request.MasterReset {
			d.restart(connected)
			return nil
		}

		// Nothing to erase: the configuration belongs to the application.
		d.restart(false)

		return apdu.RestartResponse{}

	case apdu.AuthorizeRequest:
		// No access protection: every key grants full access.
		return apdu.AuthorizeResponse{Level: 0}

	case apdu.KeyWrite:
		return apdu.KeyResponse{Level: 0xFF} // keys cannot be changed

	case apdu.MemoryRead:
		// The device has no memory that ETS could use; an empty response rejects the read.
		return apdu.MemoryResponse{Address: request.Address}

	case apdu.ADCRead:
		return apdu.ADCResponse{Channel: request.Channel} // count 0: no ADC
	}

	return nil
}

// Property IDs of the device object (interface object 0).
const (
	pidObjectType       = 1
	pidFirmwareRevision = 9
	pidSerialNumber     = 11
	pidManufacturerID   = 12
	pidOrderInfo        = 15
	pidProgMode         = 54
	pidMaxAPDULength    = 56
	pidSubnetAddr       = 57
	pidDeviceAddr       = 58
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
	pdtBitset8      = 0x33
)

// deviceProperties lists the properties of the device object in index order.
var deviceProperties = []struct {
	id       uint8
	pdt      uint8
	writable bool
}{
	{pidObjectType, pdtUnsignedInt, false},
	{pidFirmwareRevision, pdtUnsignedChar, false},
	{pidSerialNumber, pdtGeneric06, false},
	{pidManufacturerID, pdtUnsignedInt, false},
	{pidOrderInfo, pdtGeneric10, false},
	{pidProgMode, pdtBitset8, true},
	{pidMaxAPDULength, pdtUnsignedInt, false},
	{pidSubnetAddr, pdtUnsignedChar, false},
	{pidDeviceAddr, pdtUnsignedChar, false},
	{pidHardwareType, pdtGeneric06, false},
	{pidDeviceDescriptor, pdtGeneric02, false},
}

// propertyValue returns the value of a device object property. The lock is held.
func (d *Device) propertyValue(id uint8) ([]byte, bool) {
	ident := d.identity

	switch id {
	case pidObjectType:
		return []byte{0, 0}, true
	case pidFirmwareRevision:
		return []byte{ident.FirmwareRevision}, true
	case pidSerialNumber:
		return ident.SerialNumber[:], true
	case pidManufacturerID:
		return ident.SerialNumber[:2], true
	case pidOrderInfo:
		order := make([]byte, 10)
		copy(order, ident.OrderNumber)
		return order, true
	case pidProgMode:
		if d.progMode {
			return []byte{1}, true
		}
		return []byte{0}, true
	case pidMaxAPDULength:
		return []byte{0, 15}, true // standard frames
	case pidSubnetAddr:
		return []byte{byte(d.address >> 8)}, true
	case pidDeviceAddr:
		return []byte{byte(d.address)}, true
	case pidHardwareType:
		return ident.HardwareType[:], true
	case pidDeviceDescriptor:
		return []byte{byte(ident.MaskVersion >> 8), byte(ident.MaskVersion)}, true
	}

	return nil, false
}

func rejected(ref apdu.PropertyRef) apdu.PropertyValueResponse {
	ref.Count = 0
	return apdu.PropertyValueResponse{PropertyRef: ref}
}

func (d *Device) readProperty(ref apdu.PropertyRef) apdu.APDU {
	if ref.Object != 0 || ref.Count != 1 {
		return rejected(ref)
	}

	value, ok := d.propertyValue(ref.PropertyID)

	switch {
	case !ok:
		return rejected(ref)

	case ref.Start == 0: // index 0 holds the number of elements
		return apdu.PropertyValueResponse{PropertyRef: ref, Data: []byte{0, 1}}

	case ref.Start == 1:
		return apdu.PropertyValueResponse{PropertyRef: ref, Data: append([]byte(nil), value...)}
	}

	return rejected(ref)
}

func (d *Device) writeProperty(ref apdu.PropertyRef, data []byte) apdu.APDU {
	if ref.Object != 0 || ref.PropertyID != pidProgMode || ref.Count != 1 || ref.Start != 1 || len(data) < 1 {
		return rejected(ref)
	}

	d.setProgMode(data[0]&1 == 1)

	return d.readProperty(ref)
}

func (d *Device) describeProperty(request apdu.PropertyDescriptionRead) apdu.APDU {
	response := apdu.PropertyDescriptionResponse{
		Object:     request.Object,
		PropertyID: request.PropertyID,
		Index:      request.Index,
	}

	if request.Object != 0 {
		return response // MaxElements 0: no such property
	}

	for i, p := range deviceProperties {
		if (request.PropertyID != 0 && p.id == request.PropertyID) ||
			(request.PropertyID == 0 && i == int(request.Index)) {
			response.PropertyID = p.id
			response.Index = uint8(i)
			response.Type = p.pdt
			response.WriteEnable = p.writable
			response.MaxElements = 1
			response.ReadLevel = 3

			if p.writable {
				response.WriteLevel = 3
			}

			return response
		}
	}

	return response
}
