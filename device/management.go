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
		// An empty response rejects the read.
		return apdu.MemoryResponse{Address: request.Address, Data: d.readMemory(uint32(request.Address), request.Count)}

	case apdu.MemoryWrite:
		ok := d.memory != nil && d.memory.WriteMemory(uint32(request.Address), request.Data)

		// In verify mode, ETS expects the written data back.
		if d.deviceControl&deviceControlVerify != 0 {
			var data []byte
			if ok {
				data = d.readMemory(uint32(request.Address), uint8(len(request.Data)))
			}

			return apdu.MemoryResponse{Address: request.Address, Data: data}
		}

	case apdu.MemoryExtRead:
		data := d.readMemory(request.Address, request.Count)
		if data == nil {
			return apdu.MemoryExtReadResponse{Code: apdu.ReturnMemoryError, Address: request.Address}
		}

		return apdu.MemoryExtReadResponse{Code: apdu.ReturnSuccess, Address: request.Address, Data: data}

	case apdu.MemoryExtWrite:
		if d.memory == nil || !d.memory.WriteMemory(request.Address, request.Data) {
			return apdu.MemoryExtWriteResponse{Code: apdu.ReturnMemoryError, Address: request.Address}
		}

		return apdu.MemoryExtWriteResponse{Code: apdu.ReturnSuccess, Address: request.Address}

	case apdu.ADCRead:
		return apdu.ADCResponse{Channel: request.Channel} // count 0: no ADC
	}

	return nil
}

// readMemory reads count bytes of memory, or returns nil. The lock is held.
func (d *Device) readMemory(address uint32, count uint8) []byte {
	if d.memory == nil || count == 0 {
		return nil
	}

	data := make([]byte, count)
	if !d.memory.ReadMemory(address, data) {
		return nil
	}

	return data
}
