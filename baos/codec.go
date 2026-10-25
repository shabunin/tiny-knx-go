package baos

import "errors"

var (
	// ErrMalformed is returned for a message that is too short or does not match its service.
	ErrMalformed = errors.New("baos: malformed message")

	// ErrIDRange is returned when an ID or count does not fit into a version 1 message.
	ErrIDRange = errors.New("baos: ID out of range for protocol version 1")
)

// headerSize returns the size of the main service, subservice, start and count fields.
func (v Version) headerSize() int {
	if v == Version1 {
		return 4
	}

	return 6
}

func appendWord(b []byte, v Version, x uint16) []byte {
	if v == Version1 {
		return append(b, byte(x))
	}

	return append(b, byte(x>>8), byte(x))
}

func fitsVersion(v Version, start, count uint16) bool {
	return v != Version1 || (start <= 0xFF && count <= 0xFF)
}

// AppendRangeRequest appends a request for a range of IDs: GetServerItem, GetDatapointDescription,
// GetDatapointDescription2, GetDescriptionString and GetParameterByte.
func AppendRangeRequest(b []byte, v Version, service byte, start, count uint16) ([]byte, error) {
	if !fitsVersion(v, start, count) {
		return b, ErrIDRange
	}

	b = append(b, MainService, service)
	b = appendWord(b, v, start)

	return appendWord(b, v, count), nil
}

// AppendGetDatapointValue appends a GetDatapointValue request. Version 1 has no filter.
func AppendGetDatapointValue(b []byte, v Version, start, count uint16, filter Filter) ([]byte, error) {
	b, err := AppendRangeRequest(b, v, GetDatapointValue, start, count)
	if err != nil || v == Version1 {
		return b, err
	}

	return append(b, byte(filter)), nil
}

// AppendSetServerItem appends a SetServerItem request for the items, which must have ascending IDs.
func AppendSetServerItem(b []byte, v Version, items []Item) ([]byte, error) {
	if len(items) == 0 {
		return b, ErrMalformed
	}

	b, err := AppendRangeRequest(b, v, SetServerItem, items[0].ID, uint16(len(items)))
	if err != nil {
		return b, err
	}

	for _, item := range items {
		if !fitsVersion(v, item.ID, 0) || len(item.Data) > 0xFF {
			return b, ErrIDRange
		}

		b = appendWord(b, v, item.ID)
		b = append(b, byte(len(item.Data)))
		b = append(b, item.Data...)
	}

	return b, nil
}

// AppendSetDatapointValue appends a SetDatapointValue request for the datapoints, which must have
// ascending IDs.
func AppendSetDatapointValue(b []byte, v Version, values []SetValue) ([]byte, error) {
	if len(values) == 0 {
		return b, ErrMalformed
	}

	b, err := AppendRangeRequest(b, v, SetDatapointValue, values[0].ID, uint16(len(values)))
	if err != nil {
		return b, err
	}

	for _, value := range values {
		if len(value.Data) > 14 || !fitsVersion(v, value.ID, 0) {
			return b, ErrIDRange
		}

		b = appendWord(b, v, value.ID)

		// Version 1 packs the command and the length into one byte.
		if v == Version1 {
			b = append(b, byte(value.Command)<<4|byte(len(value.Data)))
		} else {
			b = append(b, byte(value.Command)&0x0F, byte(len(value.Data)))
		}

		b = append(b, value.Data...)
	}

	return b, nil
}

// AppendSetParameterByte appends a SetParameterByte request (version 2.2 and later). Without
// data, it asks the server to save the written parameters.
func AppendSetParameterByte(b []byte, start uint16, data []byte) []byte {
	b, _ = AppendRangeRequest(b, Version2, SetParameterByte, start, uint16(len(data)))
	return append(b, data...)
}

// A Message is a response or an indication, split into its header and the data that the methods
// for its service decode.
type Message struct {
	Service byte // subservice
	Start   uint16
	Count   uint16
	Body    []byte
}

// Parse splits a response or an indication of the given version.
func Parse(v Version, payload []byte) (Message, error) {
	if len(payload) < v.headerSize() || payload[0] != MainService {
		return Message{}, ErrMalformed
	}

	m := Message{Service: payload[1], Body: payload[v.headerSize():]}

	if v == Version1 {
		m.Start, m.Count = uint16(payload[2]), uint16(payload[3])
	} else {
		m.Start = uint16(payload[2])<<8 | uint16(payload[3])
		m.Count = uint16(payload[4])<<8 | uint16(payload[5])
	}

	return m, nil
}

// Err returns the error of a negative response, or nil. Negative responses have no elements and an
// error code; positive responses to writes have the error code 0.
func (m *Message) Err() error {
	if m.Count == 0 && len(m.Body) == 1 && m.Body[0] != 0 {
		return ErrorCode(m.Body[0])
	}

	return nil
}

// reader decodes the elements of a message body.
type reader struct {
	v    Version
	data []byte
	err  error
}

func (r *reader) byte() byte {
	if len(r.data) < 1 {
		r.err = ErrMalformed
		return 0
	}

	b := r.data[0]
	r.data = r.data[1:]

	return b
}

func (r *reader) word() uint16 {
	if r.v == Version1 {
		return uint16(r.byte())
	}

	return uint16(r.byte())<<8 | uint16(r.byte())
}

func (r *reader) bytes(n int) []byte {
	if len(r.data) < n {
		r.err = ErrMalformed
		return nil
	}

	b := append([]byte(nil), r.data[:n]...)
	r.data = r.data[n:]

	return b
}

// Items decodes the server items of a GetServerItem response or a ServerItem indication.
func (m *Message) Items(v Version) ([]Item, error) {
	if err := m.Err(); err != nil {
		return nil, err
	}

	r := reader{v: v, data: m.Body}
	items := make([]Item, 0, m.Count)

	for i := 0; i < int(m.Count) && r.err == nil; i++ {
		id := r.word()
		items = append(items, Item{ID: id, Data: r.bytes(int(r.byte()))})
	}

	return items, r.err
}

// Descriptions decodes the datapoint descriptions of a GetDatapointDescription response, or of a
// GetDatapointDescription2 response in version 1. Version 1 has no IDs in the response: the
// descriptions follow each other from Start. GetDatapointDescription of version 1 has no DPT.
func (m *Message) Descriptions(v Version) ([]Description, error) {
	if err := m.Err(); err != nil {
		return nil, err
	}

	r := reader{v: v, data: m.Body}
	descriptions := make([]Description, 0, m.Count)
	withDPT := v == Version2 || m.Service == GetDatapointDescription2|ResponseFlag

	for i := 0; i < int(m.Count) && r.err == nil; i++ {
		d := Description{ID: m.Start + uint16(i)}
		if v == Version2 {
			d.ID = r.word()
		}

		d.ValueType = ValueType(r.byte())
		d.Flags = Flags(r.byte())

		if withDPT {
			d.DPT = DPT(r.byte())
		}

		descriptions = append(descriptions, d)
	}

	return descriptions, r.err
}

// Values decodes the datapoint values of a GetDatapointValue response or a DatapointValue
// indication.
func (m *Message) Values(v Version) ([]Value, error) {
	if err := m.Err(); err != nil {
		return nil, err
	}

	r := reader{v: v, data: m.Body}
	values := make([]Value, 0, m.Count)

	for i := 0; i < int(m.Count) && r.err == nil; i++ {
		value := Value{ID: r.word()}

		var length int

		if v == Version1 {
			b := r.byte()
			value.State = State{
				Valid:        true,
				Updated:      b&0x80 != 0,
				ReadRequest:  b&0x40 != 0,
				Transmission: TransmissionStatus(b >> 4 & 0x03),
			}
			length = int(b & 0x0F)
		} else {
			b := r.byte()
			value.State = State{
				Valid:        b&0x10 != 0,
				Updated:      b&0x08 != 0,
				ReadRequest:  b&0x04 != 0,
				Transmission: TransmissionStatus(b & 0x03),
			}
			length = int(r.byte())
		}

		value.Data = r.bytes(length)
		values = append(values, value)
	}

	return values, r.err
}

// Strings decodes the description strings of a GetDescriptionString response. In version 2 every
// string has its length; in version 1 they all have the length of ItemDescriptionLength.
func (m *Message) Strings(v Version, length int) ([]string, error) {
	if err := m.Err(); err != nil {
		return nil, err
	}

	r := reader{v: Version2, data: m.Body}
	strings := make([]string, 0, m.Count)

	for i := 0; i < int(m.Count) && r.err == nil; i++ {
		n := length
		if v == Version2 {
			n = int(r.word())
		}

		strings = append(strings, string(r.bytes(n)))
	}

	return strings, r.err
}

// ParameterBytes returns the parameter bytes of a GetParameterByte response.
func (m *Message) ParameterBytes() ([]byte, error) {
	if err := m.Err(); err != nil {
		return nil, err
	}

	if len(m.Body) < int(m.Count) {
		return nil, ErrMalformed
	}

	return append([]byte(nil), m.Body[:m.Count]...), nil
}
