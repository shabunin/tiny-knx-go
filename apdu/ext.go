package apdu

// The extended memory services address up to 16 MB of device memory with 24-bit addresses.

func init() {
	decoders[ServiceMemoryExtWrite] = decodeMemoryExtWrite
	decoders[ServiceMemoryExtWriteResponse] = decodeMemoryExtWriteResponse
	decoders[ServiceMemoryExtRead] = decodeMemoryExtRead
	decoders[ServiceMemoryExtReadResponse] = decodeMemoryExtReadResponse
}

// Return codes of the extended services.
const (
	ReturnSuccess     = 0x00
	ReturnMemoryError = 0xF1 // the memory cannot be accessed
)

// MemoryExtWrite writes device memory at a 24-bit address.
type MemoryExtWrite struct {
	Address uint32
	Data    []byte
}

// Raw returns the encoded form of the service.
func (m MemoryExtWrite) Raw() Raw {
	return Raw{Service: ServiceMemoryExtWrite, Data: append(put24([]byte{uint8(len(m.Data))}, m.Address), m.Data...)}
}

func decodeMemoryExtWrite(raw Raw) (APDU, error) {
	if len(raw.Data) < 4 || len(raw.Data) < 4+int(raw.Data[0]) {
		return nil, ErrTooShort
	}

	return MemoryExtWrite{Address: be24(raw.Data[1:]), Data: raw.Data[4 : 4+int(raw.Data[0])]}, nil
}

// MemoryExtWriteResponse answers a MemoryExtWrite.
type MemoryExtWriteResponse struct {
	Code    uint8 // ReturnSuccess, ReturnMemoryError, …
	Address uint32
}

// Raw returns the encoded form of the service.
func (m MemoryExtWriteResponse) Raw() Raw {
	return Raw{Service: ServiceMemoryExtWriteResponse, Data: put24([]byte{m.Code}, m.Address)}
}

func decodeMemoryExtWriteResponse(raw Raw) (APDU, error) {
	if len(raw.Data) < 4 {
		return nil, ErrTooShort
	}

	return MemoryExtWriteResponse{Code: raw.Data[0], Address: be24(raw.Data[1:])}, nil
}

// MemoryExtRead reads device memory at a 24-bit address.
type MemoryExtRead struct {
	Count   uint8
	Address uint32
}

// Raw returns the encoded form of the service.
func (m MemoryExtRead) Raw() Raw {
	return Raw{Service: ServiceMemoryExtRead, Data: put24([]byte{m.Count}, m.Address)}
}

func decodeMemoryExtRead(raw Raw) (APDU, error) {
	if len(raw.Data) < 4 {
		return nil, ErrTooShort
	}

	return MemoryExtRead{Count: raw.Data[0], Address: be24(raw.Data[1:])}, nil
}

// MemoryExtReadResponse answers a MemoryExtRead. Data is empty unless Code is ReturnSuccess.
type MemoryExtReadResponse struct {
	Code    uint8
	Address uint32
	Data    []byte
}

// Raw returns the encoded form of the service.
func (m MemoryExtReadResponse) Raw() Raw {
	return Raw{Service: ServiceMemoryExtReadResponse, Data: append(put24([]byte{m.Code}, m.Address), m.Data...)}
}

func decodeMemoryExtReadResponse(raw Raw) (APDU, error) {
	if len(raw.Data) < 4 {
		return nil, ErrTooShort
	}

	return MemoryExtReadResponse{Code: raw.Data[0], Address: be24(raw.Data[1:]), Data: raw.Data[4:]}, nil
}

func be24(data []byte) uint32 {
	return uint32(data[0])<<16 | uint32(data[1])<<8 | uint32(data[2])
}

func put24(data []byte, v uint32) []byte {
	return append(data, byte(v>>16), byte(v>>8), byte(v))
}
