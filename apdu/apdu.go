// Package apdu encodes and decodes KNX application layer services (APDUs).
//
// An APDU travels in a cemi.AppData: a 4-bit APCI in Command, followed by the APCI byte whose low
// 6 bits either carry data (e.g. a switch value, a byte count) or extend the APCI to 10 bits.
// Decode turns a cemi.AppData into one of the typed services in this package; Encode does the
// reverse.
package apdu

import (
	"errors"

	"github.com/shabunin/tiny-knx-go/cemi"
)

// ErrTooShort is returned when an APDU is shorter than its service requires.
var ErrTooShort = errors.New("apdu: too short")

// Service identifies an application layer service by its 10-bit APCI. For services with a 4-bit
// APCI, the low 6 bits are zero and the 6 bits travel as data (see Raw.Short).
type Service uint16

// These are the application layer services. Values follow KNX 03_03_07.
const (
	ServiceGroupValueRead                        Service = 0x000
	ServiceGroupValueResponse                    Service = 0x040
	ServiceGroupValueWrite                       Service = 0x080
	ServiceIndividualAddressWrite                Service = 0x0C0
	ServiceIndividualAddressRead                 Service = 0x100
	ServiceIndividualAddressResponse             Service = 0x140
	ServiceADCRead                               Service = 0x180
	ServiceADCResponse                           Service = 0x1C0
	ServiceSystemNetworkParameterRead            Service = 0x1C8
	ServiceSystemNetworkParameterResponse        Service = 0x1C9
	ServiceSystemNetworkParameterWrite           Service = 0x1CA
	ServicePropertyValueExtRead                  Service = 0x1CC
	ServicePropertyValueExtResponse              Service = 0x1CD
	ServicePropertyValueExtWriteCon              Service = 0x1CE
	ServicePropertyValueExtWriteConResponse      Service = 0x1CF
	ServicePropertyValueExtWriteUnCon            Service = 0x1D0
	ServicePropertyExtDescriptionRead            Service = 0x1D2
	ServicePropertyExtDescriptionResponse        Service = 0x1D3
	ServiceFunctionPropertyExtCommand            Service = 0x1D4
	ServiceFunctionPropertyExtState              Service = 0x1D5
	ServiceFunctionPropertyExtStateResponse      Service = 0x1D6
	ServiceMemoryExtWrite                        Service = 0x1FB
	ServiceMemoryExtWriteResponse                Service = 0x1FC
	ServiceMemoryExtRead                         Service = 0x1FD
	ServiceMemoryExtReadResponse                 Service = 0x1FE
	ServiceMemoryRead                            Service = 0x200
	ServiceMemoryResponse                        Service = 0x240
	ServiceMemoryWrite                           Service = 0x280
	ServiceUserMemoryRead                        Service = 0x2C0
	ServiceUserMemoryResponse                    Service = 0x2C1
	ServiceUserMemoryWrite                       Service = 0x2C2
	ServiceUserManufacturerInfoRead              Service = 0x2C5
	ServiceUserManufacturerInfoResponse          Service = 0x2C6
	ServiceFunctionPropertyCommand               Service = 0x2C7
	ServiceFunctionPropertyState                 Service = 0x2C8
	ServiceFunctionPropertyStateResponse         Service = 0x2C9
	ServiceDeviceDescriptorRead                  Service = 0x300
	ServiceDeviceDescriptorResponse              Service = 0x340
	ServiceRestart                               Service = 0x380
	ServiceAuthorizeRequest                      Service = 0x3D1
	ServiceAuthorizeResponse                     Service = 0x3D2
	ServiceKeyWrite                              Service = 0x3D3
	ServiceKeyResponse                           Service = 0x3D4
	ServicePropertyValueRead                     Service = 0x3D5
	ServicePropertyValueResponse                 Service = 0x3D6
	ServicePropertyValueWrite                    Service = 0x3D7
	ServicePropertyDescriptionRead               Service = 0x3D8
	ServicePropertyDescriptionResponse           Service = 0x3D9
	ServiceNetworkParameterRead                  Service = 0x3DA
	ServiceNetworkParameterResponse              Service = 0x3DB
	ServiceIndividualAddressSerialNumberRead     Service = 0x3DC
	ServiceIndividualAddressSerialNumberResponse Service = 0x3DD
	ServiceIndividualAddressSerialNumberWrite    Service = 0x3DE
	ServiceDomainAddressWrite                    Service = 0x3E0
	ServiceDomainAddressRead                     Service = 0x3E1
	ServiceDomainAddressResponse                 Service = 0x3E2
	ServiceDomainAddressSelectiveRead            Service = 0x3E3
	ServiceNetworkParameterWrite                 Service = 0x3E4
	ServiceDomainAddressSerialNumberRead         Service = 0x3EC
	ServiceDomainAddressSerialNumberResponse     Service = 0x3ED
	ServiceDomainAddressSerialNumberWrite        Service = 0x3EE
	ServiceSecure                                Service = 0x3F1
)

// is10Bit reports whether a 4-bit APCI and the following 6 bits form a 10-bit service.
//
// UserMessage (1011) and Escape (1111) are always extended. ADCResponse (0111) shares its code with
// the 10-bit services 0x1C8 and up; ADC channels are 0–7, so larger values are extended services.
func is10Bit(apci4 cemi.APCI, low6 uint8) bool {
	switch apci4 {
	case cemi.UserMessage, cemi.Escape:
		return true
	case cemi.AdcResponse:
		return low6 >= 8
	}

	return false
}

// Raw is an APDU split into its parts.
type Raw struct {
	Service Service

	// Short holds the 6 bits that follow a 4-bit APCI. It is always 0 for 10-bit services.
	Short uint8

	// Data holds the bytes after the APCI byte.
	Data []byte
}

// RawFrom splits the application data of a transport unit into its parts.
func RawFrom(app *cemi.AppData) Raw {
	var low6 uint8
	var data []byte

	if len(app.Data) > 0 {
		low6 = app.Data[0] & 0x3F
		data = app.Data[1:]
	}

	if is10Bit(app.Command, low6) {
		return Raw{Service: Service(app.Command)<<6 | Service(low6), Data: data}
	}

	return Raw{Service: Service(app.Command) << 6, Short: low6, Data: data}
}

// AppData returns the raw APDU as unnumbered application data.
func (raw Raw) AppData() *cemi.AppData {
	low6 := raw.Short & 0x3F
	if raw.Service&0x3F != 0 {
		low6 = uint8(raw.Service & 0x3F)
	}

	data := make([]byte, 1+len(raw.Data))
	data[0] = low6
	copy(data[1:], raw.Data)

	return &cemi.AppData{Command: cemi.APCI(raw.Service >> 6), Data: data}
}

// An APDU is an application layer service.
type APDU interface {
	// Raw returns the encoded form of the service.
	Raw() Raw
}

// Encode returns the APDU as unnumbered application data, ready to be put into an L_Data frame.
func Encode(apdu APDU) *cemi.AppData {
	return apdu.Raw().AppData()
}

// Decode parses application data into a typed service. Services without a type in this package
// are returned as Unknown.
func Decode(app *cemi.AppData) (APDU, error) {
	return DecodeRaw(RawFrom(app))
}

// DecodeRaw parses a raw APDU into a typed service.
func DecodeRaw(raw Raw) (APDU, error) {
	decode, ok := decoders[raw.Service]
	if !ok {
		return Unknown(raw), nil
	}

	return decode(raw)
}

// Unknown is a service without a type in this package.
type Unknown Raw

// Raw returns the encoded form of the service.
func (u Unknown) Raw() Raw { return Raw(u) }

var decoders = map[Service]func(Raw) (APDU, error){
	ServiceGroupValueRead:                        decodeGroupValueRead,
	ServiceGroupValueResponse:                    decodeGroupValueResponse,
	ServiceGroupValueWrite:                       decodeGroupValueWrite,
	ServiceIndividualAddressWrite:                decodeIndividualAddressWrite,
	ServiceIndividualAddressRead:                 decodeIndividualAddressRead,
	ServiceIndividualAddressResponse:             decodeIndividualAddressResponse,
	ServiceIndividualAddressSerialNumberRead:     decodeIndividualAddressSerialNumberRead,
	ServiceIndividualAddressSerialNumberResponse: decodeIndividualAddressSerialNumberResponse,
	ServiceIndividualAddressSerialNumberWrite:    decodeIndividualAddressSerialNumberWrite,
	ServiceADCRead:                               decodeADCRead,
	ServiceADCResponse:                           decodeADCResponse,
	ServiceMemoryRead:                            decodeMemoryRead,
	ServiceMemoryResponse:                        decodeMemoryResponse,
	ServiceMemoryWrite:                           decodeMemoryWrite,
	ServiceUserManufacturerInfoRead:              decodeUserManufacturerInfoRead,
	ServiceUserManufacturerInfoResponse:          decodeUserManufacturerInfoResponse,
	ServiceDeviceDescriptorRead:                  decodeDeviceDescriptorRead,
	ServiceDeviceDescriptorResponse:              decodeDeviceDescriptorResponse,
	ServiceRestart:                               decodeRestart,
	ServiceAuthorizeRequest:                      decodeAuthorizeRequest,
	ServiceAuthorizeResponse:                     decodeAuthorizeResponse,
	ServiceKeyWrite:                              decodeKeyWrite,
	ServiceKeyResponse:                           decodeKeyResponse,
	ServicePropertyValueRead:                     decodePropertyValueRead,
	ServicePropertyValueResponse:                 decodePropertyValueResponse,
	ServicePropertyValueWrite:                    decodePropertyValueWrite,
	ServicePropertyDescriptionRead:               decodePropertyDescriptionRead,
	ServicePropertyDescriptionResponse:           decodePropertyDescriptionResponse,
}

func be16(data []byte) uint16 {
	return uint16(data[0])<<8 | uint16(data[1])
}

func be32(data []byte) uint32 {
	return uint32(data[0])<<24 | uint32(data[1])<<16 | uint32(data[2])<<8 | uint32(data[3])
}

func put16(data []byte, v uint16) []byte {
	return append(data, byte(v>>8), byte(v))
}

func put32(data []byte, v uint32) []byte {
	return append(data, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}
