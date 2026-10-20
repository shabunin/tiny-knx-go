package apdu

import "github.com/shabunin/tiny-knx-go/cemi"

// Group values use the same layout as dpt Pack: the first byte holds a value of up to 6 bits (or
// is 0), longer values follow it.

// GroupValueRead requests the value of a group object.
type GroupValueRead struct{}

// Raw returns the encoded form of the service.
func (GroupValueRead) Raw() Raw { return Raw{Service: ServiceGroupValueRead} }

func decodeGroupValueRead(Raw) (APDU, error) { return GroupValueRead{}, nil }

// GroupValueResponse answers a GroupValueRead.
type GroupValueResponse struct {
	Data []byte
}

// Raw returns the encoded form of the service.
func (m GroupValueResponse) Raw() Raw { return groupValueRaw(ServiceGroupValueResponse, m.Data) }

func decodeGroupValueResponse(raw Raw) (APDU, error) {
	return GroupValueResponse{Data: groupValueData(raw)}, nil
}

// GroupValueWrite sets the value of a group object.
type GroupValueWrite struct {
	Data []byte
}

// Raw returns the encoded form of the service.
func (m GroupValueWrite) Raw() Raw { return groupValueRaw(ServiceGroupValueWrite, m.Data) }

func decodeGroupValueWrite(raw Raw) (APDU, error) {
	return GroupValueWrite{Data: groupValueData(raw)}, nil
}

func groupValueRaw(service Service, data []byte) Raw {
	if len(data) == 0 {
		return Raw{Service: service}
	}

	return Raw{Service: service, Short: data[0] & 0x3F, Data: data[1:]}
}

func groupValueData(raw Raw) []byte {
	return append([]byte{raw.Short}, raw.Data...)
}

// IndividualAddressWrite sets the individual address of all devices in programming mode.
type IndividualAddressWrite struct {
	Address cemi.IndividualAddr
}

// Raw returns the encoded form of the service.
func (m IndividualAddressWrite) Raw() Raw {
	return Raw{Service: ServiceIndividualAddressWrite, Data: put16(nil, uint16(m.Address))}
}

func decodeIndividualAddressWrite(raw Raw) (APDU, error) {
	if len(raw.Data) < 2 {
		return nil, ErrTooShort
	}

	return IndividualAddressWrite{Address: cemi.IndividualAddr(be16(raw.Data))}, nil
}

// IndividualAddressRead asks devices in programming mode for their individual address.
type IndividualAddressRead struct{}

// Raw returns the encoded form of the service.
func (IndividualAddressRead) Raw() Raw { return Raw{Service: ServiceIndividualAddressRead} }

func decodeIndividualAddressRead(Raw) (APDU, error) { return IndividualAddressRead{}, nil }

// IndividualAddressResponse answers an IndividualAddressRead. The address is the frame's source.
type IndividualAddressResponse struct{}

// Raw returns the encoded form of the service.
func (IndividualAddressResponse) Raw() Raw { return Raw{Service: ServiceIndividualAddressResponse} }

func decodeIndividualAddressResponse(Raw) (APDU, error) { return IndividualAddressResponse{}, nil }

// SerialNumber is the 6-byte KNX serial number of a device.
type SerialNumber [6]byte

// IndividualAddressSerialNumberRead asks the device with the given serial number for its address.
type IndividualAddressSerialNumberRead struct {
	SerialNumber SerialNumber
}

// Raw returns the encoded form of the service.
func (m IndividualAddressSerialNumberRead) Raw() Raw {
	return Raw{Service: ServiceIndividualAddressSerialNumberRead, Data: append([]byte(nil), m.SerialNumber[:]...)}
}

func decodeIndividualAddressSerialNumberRead(raw Raw) (APDU, error) {
	var m IndividualAddressSerialNumberRead
	if copy(m.SerialNumber[:], raw.Data) < 6 {
		return nil, ErrTooShort
	}

	return m, nil
}

// IndividualAddressSerialNumberResponse answers an IndividualAddressSerialNumberRead.
type IndividualAddressSerialNumberResponse struct {
	SerialNumber  SerialNumber
	DomainAddress uint16
}

// Raw returns the encoded form of the service.
func (m IndividualAddressSerialNumberResponse) Raw() Raw {
	data := append([]byte(nil), m.SerialNumber[:]...)

	return Raw{Service: ServiceIndividualAddressSerialNumberResponse, Data: put16(data, m.DomainAddress)}
}

func decodeIndividualAddressSerialNumberResponse(raw Raw) (APDU, error) {
	if len(raw.Data) < 8 {
		return nil, ErrTooShort
	}

	var m IndividualAddressSerialNumberResponse
	copy(m.SerialNumber[:], raw.Data)
	m.DomainAddress = be16(raw.Data[6:])

	return m, nil
}

// IndividualAddressSerialNumberWrite sets the address of the device with the given serial number.
type IndividualAddressSerialNumberWrite struct {
	SerialNumber SerialNumber
	Address      cemi.IndividualAddr
}

// Raw returns the encoded form of the service.
func (m IndividualAddressSerialNumberWrite) Raw() Raw {
	data := append([]byte(nil), m.SerialNumber[:]...)
	data = put16(data, uint16(m.Address))
	data = append(data, 0, 0, 0, 0) // reserved

	return Raw{Service: ServiceIndividualAddressSerialNumberWrite, Data: data}
}

func decodeIndividualAddressSerialNumberWrite(raw Raw) (APDU, error) {
	if len(raw.Data) < 8 {
		return nil, ErrTooShort
	}

	var m IndividualAddressSerialNumberWrite
	copy(m.SerialNumber[:], raw.Data)
	m.Address = cemi.IndividualAddr(be16(raw.Data[6:]))

	return m, nil
}

// ADCRead reads an analog channel, e.g. the bus voltage (channel 1).
type ADCRead struct {
	Channel uint8
	Count   uint8
}

// Raw returns the encoded form of the service.
func (m ADCRead) Raw() Raw {
	return Raw{Service: ServiceADCRead, Short: m.Channel, Data: []byte{m.Count}}
}

func decodeADCRead(raw Raw) (APDU, error) {
	if len(raw.Data) < 1 {
		return nil, ErrTooShort
	}

	return ADCRead{Channel: raw.Short, Count: raw.Data[0]}, nil
}

// ADCResponse answers an ADCRead. Count 0 means the read failed. Channel is 0–7; higher values
// share their code with 10-bit services.
type ADCResponse struct {
	Channel uint8
	Count   uint8
	Value   uint16
}

// Raw returns the encoded form of the service.
func (m ADCResponse) Raw() Raw {
	return Raw{Service: ServiceADCResponse, Short: m.Channel & 7, Data: put16([]byte{m.Count}, m.Value)}
}

func decodeADCResponse(raw Raw) (APDU, error) {
	if len(raw.Data) < 3 {
		return nil, ErrTooShort
	}

	return ADCResponse{Channel: raw.Short, Count: raw.Data[0], Value: be16(raw.Data[1:])}, nil
}

// MemoryRead reads up to 63 bytes of device memory.
type MemoryRead struct {
	Count   uint8
	Address uint16
}

// Raw returns the encoded form of the service.
func (m MemoryRead) Raw() Raw {
	return Raw{Service: ServiceMemoryRead, Short: m.Count, Data: put16(nil, m.Address)}
}

func decodeMemoryRead(raw Raw) (APDU, error) {
	if len(raw.Data) < 2 {
		return nil, ErrTooShort
	}

	return MemoryRead{Count: raw.Short, Address: be16(raw.Data)}, nil
}

// MemoryResponse answers a MemoryRead. An empty Data means the read was rejected.
type MemoryResponse struct {
	Address uint16
	Data    []byte
}

// Raw returns the encoded form of the service.
func (m MemoryResponse) Raw() Raw {
	return memoryRaw(ServiceMemoryResponse, m.Address, m.Data)
}

func decodeMemoryResponse(raw Raw) (APDU, error) {
	address, data, err := memoryData(raw)
	return MemoryResponse{Address: address, Data: data}, err
}

// MemoryWrite writes up to 63 bytes of device memory.
type MemoryWrite struct {
	Address uint16
	Data    []byte
}

// Raw returns the encoded form of the service.
func (m MemoryWrite) Raw() Raw {
	return memoryRaw(ServiceMemoryWrite, m.Address, m.Data)
}

func decodeMemoryWrite(raw Raw) (APDU, error) {
	address, data, err := memoryData(raw)
	return MemoryWrite{Address: address, Data: data}, err
}

func memoryRaw(service Service, address uint16, data []byte) Raw {
	return Raw{Service: service, Short: uint8(len(data)), Data: append(put16(nil, address), data...)}
}

func memoryData(raw Raw) (uint16, []byte, error) {
	if len(raw.Data) < 2+int(raw.Short) {
		return 0, nil, ErrTooShort
	}

	return be16(raw.Data), raw.Data[2 : 2+int(raw.Short)], nil
}

// UserManufacturerInfoRead asks for the manufacturer of the user application.
type UserManufacturerInfoRead struct{}

// Raw returns the encoded form of the service.
func (UserManufacturerInfoRead) Raw() Raw { return Raw{Service: ServiceUserManufacturerInfoRead} }

func decodeUserManufacturerInfoRead(Raw) (APDU, error) { return UserManufacturerInfoRead{}, nil }

// UserManufacturerInfoResponse answers a UserManufacturerInfoRead.
type UserManufacturerInfoResponse struct {
	Manufacturer uint8
	Data         [2]byte
}

// Raw returns the encoded form of the service.
func (m UserManufacturerInfoResponse) Raw() Raw {
	return Raw{Service: ServiceUserManufacturerInfoResponse, Data: []byte{m.Manufacturer, m.Data[0], m.Data[1]}}
}

func decodeUserManufacturerInfoResponse(raw Raw) (APDU, error) {
	if len(raw.Data) < 3 {
		return nil, ErrTooShort
	}

	return UserManufacturerInfoResponse{Manufacturer: raw.Data[0], Data: [2]byte{raw.Data[1], raw.Data[2]}}, nil
}

// DeviceDescriptorRead reads a device descriptor. Type 0 is the mask version.
type DeviceDescriptorRead struct {
	Type uint8
}

// Raw returns the encoded form of the service.
func (m DeviceDescriptorRead) Raw() Raw {
	return Raw{Service: ServiceDeviceDescriptorRead, Short: m.Type}
}

func decodeDeviceDescriptorRead(raw Raw) (APDU, error) {
	return DeviceDescriptorRead{Type: raw.Short}, nil
}

// DeviceDescriptorResponse answers a DeviceDescriptorRead. Type 0x3F means the descriptor type is
// not supported.
type DeviceDescriptorResponse struct {
	Type       uint8
	Descriptor []byte
}

// Raw returns the encoded form of the service.
func (m DeviceDescriptorResponse) Raw() Raw {
	return Raw{Service: ServiceDeviceDescriptorResponse, Short: m.Type, Data: m.Descriptor}
}

func decodeDeviceDescriptorResponse(raw Raw) (APDU, error) {
	return DeviceDescriptorResponse{Type: raw.Short, Descriptor: raw.Data}, nil
}

// Restart bits in the 6 bits after the APCI.
const (
	restartMaster   = 0x01
	restartResponse = 0x20
)

// Restart restarts a device. A master reset can additionally erase parts of its configuration.
type Restart struct {
	MasterReset bool
	EraseCode   uint8
	Channel     uint8
}

// Raw returns the encoded form of the service.
func (m Restart) Raw() Raw {
	if !m.MasterReset {
		return Raw{Service: ServiceRestart}
	}

	return Raw{Service: ServiceRestart, Short: restartMaster, Data: []byte{m.EraseCode, m.Channel}}
}

// RestartResponse answers a master reset.
type RestartResponse struct {
	ErrorCode   uint8
	ProcessTime uint16 // seconds
}

// Raw returns the encoded form of the service.
func (m RestartResponse) Raw() Raw {
	return Raw{
		Service: ServiceRestart,
		Short:   restartResponse | restartMaster,
		Data:    put16([]byte{m.ErrorCode}, m.ProcessTime),
	}
}

func decodeRestart(raw Raw) (APDU, error) {
	if raw.Short&restartResponse != 0 {
		if len(raw.Data) < 3 {
			return nil, ErrTooShort
		}

		return RestartResponse{ErrorCode: raw.Data[0], ProcessTime: be16(raw.Data[1:])}, nil
	}

	if raw.Short&restartMaster == 0 {
		return Restart{}, nil
	}

	if len(raw.Data) < 2 {
		return nil, ErrTooShort
	}

	return Restart{MasterReset: true, EraseCode: raw.Data[0], Channel: raw.Data[1]}, nil
}

// AuthorizeRequest authenticates with a key to gain an access level.
type AuthorizeRequest struct {
	Key uint32
}

// Raw returns the encoded form of the service.
func (m AuthorizeRequest) Raw() Raw {
	return Raw{Service: ServiceAuthorizeRequest, Data: put32([]byte{0}, m.Key)}
}

func decodeAuthorizeRequest(raw Raw) (APDU, error) {
	if len(raw.Data) < 5 {
		return nil, ErrTooShort
	}

	return AuthorizeRequest{Key: be32(raw.Data[1:])}, nil
}

// AuthorizeResponse answers an AuthorizeRequest with the granted access level.
type AuthorizeResponse struct {
	Level uint8
}

// Raw returns the encoded form of the service.
func (m AuthorizeResponse) Raw() Raw {
	return Raw{Service: ServiceAuthorizeResponse, Data: []byte{m.Level}}
}

func decodeAuthorizeResponse(raw Raw) (APDU, error) {
	if len(raw.Data) < 1 {
		return nil, ErrTooShort
	}

	return AuthorizeResponse{Level: raw.Data[0]}, nil
}

// KeyWrite sets the key of an access level.
type KeyWrite struct {
	Level uint8
	Key   uint32
}

// Raw returns the encoded form of the service.
func (m KeyWrite) Raw() Raw {
	return Raw{Service: ServiceKeyWrite, Data: put32([]byte{m.Level}, m.Key)}
}

func decodeKeyWrite(raw Raw) (APDU, error) {
	if len(raw.Data) < 5 {
		return nil, ErrTooShort
	}

	return KeyWrite{Level: raw.Data[0], Key: be32(raw.Data[1:])}, nil
}

// KeyResponse answers a KeyWrite.
type KeyResponse struct {
	Level uint8
}

// Raw returns the encoded form of the service.
func (m KeyResponse) Raw() Raw {
	return Raw{Service: ServiceKeyResponse, Data: []byte{m.Level}}
}

func decodeKeyResponse(raw Raw) (APDU, error) {
	if len(raw.Data) < 1 {
		return nil, ErrTooShort
	}

	return KeyResponse{Level: raw.Data[0]}, nil
}

// PropertyRef addresses elements of an interface object property: Count elements (at most 15)
// starting at Start (1-based; index 0 holds the current number of elements).
type PropertyRef struct {
	Object     uint8
	PropertyID uint8
	Count      uint8
	Start      uint16
}

func (ref PropertyRef) append(data []byte) []byte {
	data = append(data, ref.Object, ref.PropertyID)
	return put16(data, uint16(ref.Count&15)<<12|ref.Start&0xFFF)
}

func decodePropertyRef(data []byte) (PropertyRef, error) {
	if len(data) < 4 {
		return PropertyRef{}, ErrTooShort
	}

	countStart := be16(data[2:])

	return PropertyRef{
		Object:     data[0],
		PropertyID: data[1],
		Count:      uint8(countStart >> 12),
		Start:      countStart & 0xFFF,
	}, nil
}

// PropertyValueRead reads property elements.
type PropertyValueRead struct {
	PropertyRef
}

// Raw returns the encoded form of the service.
func (m PropertyValueRead) Raw() Raw {
	return Raw{Service: ServicePropertyValueRead, Data: m.append(nil)}
}

func decodePropertyValueRead(raw Raw) (APDU, error) {
	ref, err := decodePropertyRef(raw.Data)
	return PropertyValueRead{ref}, err
}

// PropertyValueResponse answers a PropertyValueRead or PropertyValueWrite. Count 0 means the access
// was rejected.
type PropertyValueResponse struct {
	PropertyRef
	Data []byte
}

// Raw returns the encoded form of the service.
func (m PropertyValueResponse) Raw() Raw {
	return Raw{Service: ServicePropertyValueResponse, Data: append(m.append(nil), m.Data...)}
}

func decodePropertyValueResponse(raw Raw) (APDU, error) {
	ref, err := decodePropertyRef(raw.Data)
	if err != nil {
		return nil, err
	}

	return PropertyValueResponse{ref, raw.Data[4:]}, nil
}

// PropertyValueWrite writes property elements. The device answers with a PropertyValueResponse.
type PropertyValueWrite struct {
	PropertyRef
	Data []byte
}

// Raw returns the encoded form of the service.
func (m PropertyValueWrite) Raw() Raw {
	return Raw{Service: ServicePropertyValueWrite, Data: append(m.append(nil), m.Data...)}
}

func decodePropertyValueWrite(raw Raw) (APDU, error) {
	ref, err := decodePropertyRef(raw.Data)
	if err != nil {
		return nil, err
	}

	return PropertyValueWrite{ref, raw.Data[4:]}, nil
}

// PropertyDescriptionRead reads the description of a property, addressed either by PropertyID or,
// if PropertyID is 0, by its Index in the object.
type PropertyDescriptionRead struct {
	Object     uint8
	PropertyID uint8
	Index      uint8
}

// Raw returns the encoded form of the service.
func (m PropertyDescriptionRead) Raw() Raw {
	return Raw{Service: ServicePropertyDescriptionRead, Data: []byte{m.Object, m.PropertyID, m.Index}}
}

func decodePropertyDescriptionRead(raw Raw) (APDU, error) {
	if len(raw.Data) < 3 {
		return nil, ErrTooShort
	}

	return PropertyDescriptionRead{Object: raw.Data[0], PropertyID: raw.Data[1], Index: raw.Data[2]}, nil
}

// PropertyDescriptionResponse answers a PropertyDescriptionRead. MaxElements 0 means the property
// does not exist.
type PropertyDescriptionResponse struct {
	Object      uint8
	PropertyID  uint8
	Index       uint8
	WriteEnable bool
	Type        uint8 // property datatype, 6 bits
	MaxElements uint16
	ReadLevel   uint8
	WriteLevel  uint8
}

// Raw returns the encoded form of the service.
func (m PropertyDescriptionResponse) Raw() Raw {
	typ := m.Type & 0x3F
	if m.WriteEnable {
		typ |= 0x80
	}

	data := []byte{m.Object, m.PropertyID, m.Index, typ}
	data = put16(data, m.MaxElements&0xFFF)
	data = append(data, m.ReadLevel<<4|m.WriteLevel&15)

	return Raw{Service: ServicePropertyDescriptionResponse, Data: data}
}

func decodePropertyDescriptionResponse(raw Raw) (APDU, error) {
	if len(raw.Data) < 7 {
		return nil, ErrTooShort
	}

	data := raw.Data

	return PropertyDescriptionResponse{
		Object:      data[0],
		PropertyID:  data[1],
		Index:       data[2],
		WriteEnable: data[3]&0x80 != 0,
		Type:        data[3] & 0x3F,
		MaxElements: be16(data[4:]) & 0xFFF,
		ReadLevel:   data[6] >> 4,
		WriteLevel:  data[6] & 15,
	}, nil
}
