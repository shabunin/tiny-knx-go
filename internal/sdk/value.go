package sdk

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/shabunin/tiny-knx-go/baos"
)

// These are the errors of values that do not fit their datapoint.
var (
	ErrValueType       = errors.New("ERR_WRONG_VALUE_TYPE")
	ErrValue           = errors.New("ERR_WRONG_VALUE")
	ErrDPTNotSupported = errors.New("ERR_DPT_NOT_SUPPORTED")
)

// mainType returns the main number of the datapoint type of d. Version 1 servers without DPTs
// describe only the size, which tells a few types.
func mainType(d baos.Description) int {
	if main := d.DPT.Main(); main != 0 {
		return main
	}

	switch d.ValueType {
	case baos.Bits1:
		return 1
	case baos.Bytes1:
		return 5
	case baos.Bytes4:
		return 12
	}

	return 0
}

// decodeValue returns the value of a datapoint as JSON takes it, in the formats of dobaos, or nil
// if its type has no such format.
func decodeValue(d baos.Description, data []byte) any {
	main := mainType(d)
	if size := minSize(main); size == 0 || len(data) < size {
		return nil
	}

	switch main {
	case 1:
		return data[0]&1 != 0
	case 2:
		return map[string]bool{"control": data[0]&2 != 0, "value": data[0]&1 != 0}
	case 3:
		return map[string]int{"direction": int(data[0]>>3) & 1, "step": int(data[0] & 7)}
	case 4:
		return string(rune(data[0]))
	case 5, 20:
		return data[0]
	case 6:
		return int8(data[0])
	case 7:
		return binary.BigEndian.Uint16(data)
	case 8:
		return int16(binary.BigEndian.Uint16(data))
	case 9:
		return decodeFloat16(data)
	case 10:
		return map[string]int{"day": int(data[0] >> 5), "hour": int(data[0] & 0x1F), "minutes": int(data[1] & 0x3F), "seconds": int(data[2] & 0x3F)}
	case 11:
		year := int(data[2] & 0x7F)
		if year < 90 {
			year += 2000
		} else {
			year += 1900
		}

		return map[string]int{"day": int(data[0] & 0x1F), "month": int(data[1] & 0x0F), "year": year}
	case 12:
		return binary.BigEndian.Uint32(data)
	case 13:
		return int32(binary.BigEndian.Uint32(data))
	case 14:
		return math.Float32frombits(binary.BigEndian.Uint32(data))
	case 16:
		var b strings.Builder
		for _, c := range data[:14] {
			if c == 0 {
				break
			}

			b.WriteRune(rune(c)) // ISO 8859-1
		}

		return b.String()
	case 17:
		return data[0] & 0x3F
	case 18:
		return map[string]int{"learn": int(data[0] >> 7), "number": int(data[0] & 0x3F)}
	}

	return nil
}

// minSize returns the size of the values of a main type that decodeValue takes, or 0.
func minSize(main int) int {
	switch main {
	case 1, 2, 3, 4, 5, 6, 17, 18, 20:
		return 1
	case 7, 8, 9:
		return 2
	case 10, 11:
		return 3
	case 12, 13, 14:
		return 4
	case 16:
		return 14
	}

	return 0
}

// encodeValue returns the data of a datapoint value given as JSON decodes it: bool, float64,
// string or map[string]any.
func encodeValue(d baos.Description, v any) ([]byte, error) {
	main := mainType(d)

	switch main {
	case 1:
		b, err := toBool(v)
		return []byte{bit(b)}, err

	case 2:
		f, err := fields(v, "control", "value")
		if err != nil {
			return nil, err
		}

		control, err1 := toBool(f["control"])
		value, err2 := toBool(f["value"])

		return []byte{bit(control)<<1 | bit(value)}, errors.Join(err1, err2)

	case 3:
		f, err := fields(v, "direction", "step")
		if err != nil {
			return nil, err
		}

		direction, err1 := toBool(f["direction"])
		step, err2 := toInt(f["step"], 0, 7)

		return []byte{bit(direction)<<3 | byte(step)}, errors.Join(err1, err2)

	case 4:
		s, ok := v.(string)
		if !ok || len([]rune(s)) != 1 || []rune(s)[0] > 0xFF {
			return nil, ErrValueType
		}

		return []byte{byte([]rune(s)[0])}, nil

	case 5, 20:
		n, err := toInt(v, 0, 255)
		return []byte{byte(n)}, err

	case 6:
		n, err := toInt(v, -128, 127)
		return []byte{byte(int8(n))}, err

	case 7:
		n, err := toInt(v, 0, 65535)
		return binary.BigEndian.AppendUint16(nil, uint16(n)), err

	case 8:
		n, err := toInt(v, -32768, 32767)
		return binary.BigEndian.AppendUint16(nil, uint16(int16(n))), err

	case 9:
		f, ok := v.(float64)
		if !ok {
			return nil, ErrValueType
		}

		return encodeFloat16(f)

	case 10:
		f, err := fields(v, "day", "hour", "minutes", "seconds")
		if err != nil {
			return nil, err
		}

		day, err1 := toInt(f["day"], 0, 7)
		hour, err2 := toInt(f["hour"], 0, 23)
		minutes, err3 := toInt(f["minutes"], 0, 59)
		seconds, err4 := toInt(f["seconds"], 0, 59)

		return []byte{byte(day<<5 | hour), byte(minutes), byte(seconds)}, errors.Join(err1, err2, err3, err4)

	case 11:
		f, err := fields(v, "day", "month", "year")
		if err != nil {
			return nil, err
		}

		day, err1 := toInt(f["day"], 1, 31)
		month, err2 := toInt(f["month"], 1, 12)
		year, err3 := toInt(f["year"], 1990, 2089)

		return []byte{byte(day), byte(month), byte(year % 100)}, errors.Join(err1, err2, err3)

	case 12:
		n, err := toInt(v, 0, math.MaxUint32)
		return binary.BigEndian.AppendUint32(nil, uint32(n)), err

	case 13:
		n, err := toInt(v, math.MinInt32, math.MaxInt32)
		return binary.BigEndian.AppendUint32(nil, uint32(int32(n))), err

	case 14:
		f, ok := v.(float64)
		if !ok {
			return nil, ErrValueType
		}

		return binary.BigEndian.AppendUint32(nil, math.Float32bits(float32(f))), nil

	case 16:
		s, ok := v.(string)
		if !ok {
			return nil, ErrValueType
		}

		data := make([]byte, 14)
		for i, r := range []rune(s) {
			if i == len(data) || r > 0xFF {
				return nil, ErrValue
			}

			data[i] = byte(r)
		}

		return data, nil

	case 17:
		n, err := toInt(v, 0, 63)
		return []byte{byte(n)}, err

	case 18:
		f, err := fields(v, "learn", "number")
		if err != nil {
			return nil, err
		}

		learn, err1 := toBool(f["learn"])
		number, err2 := toInt(f["number"], 0, 63)

		return []byte{bit(learn)<<7 | byte(number)}, errors.Join(err1, err2)
	}

	return nil, ErrDPTNotSupported
}

func bit(b bool) byte {
	if b {
		return 1
	}

	return 0
}

// toBool takes true, false and numbers, which are true unless 0.
func toBool(v any) (bool, error) {
	switch v := v.(type) {
	case bool:
		return v, nil
	case float64:
		return v != 0, nil
	}

	return false, ErrValueType
}

func toInt(v any, low, high int64) (int64, error) {
	f, ok := v.(float64)
	if !ok {
		return 0, ErrValueType
	}

	if f != math.Trunc(f) || f < float64(low) || f > float64(high) {
		return 0, ErrValue
	}

	return int64(f), nil
}

func fields(v any, names ...string) (map[string]any, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, ErrValueType
	}

	for _, name := range names {
		if _, ok := m[name]; !ok {
			return nil, fmt.Errorf("%w: no %s", ErrValue, name)
		}
	}

	return m, nil
}

// decodeFloat16 decodes a 2-byte float (DPT 9): 0.01 × M × 2^E, with a sign bit and 11 bits of M
// in two's complement, rounded to the hundredths that it counts.
func decodeFloat16(data []byte) float64 {
	raw := binary.BigEndian.Uint16(data)
	m := int(raw & 0x07FF)
	if raw&0x8000 != 0 {
		m -= 2048
	}

	e := int(raw>>11) & 0x0F

	return math.Round(float64(m<<e)) / 100
}

func encodeFloat16(f float64) ([]byte, error) {
	if math.IsNaN(f) || f < -671088.64 || f > 670760.96 {
		return nil, ErrValue
	}

	m := math.Round(f * 100)
	e := 0

	for m < -2048 || m > 2047 {
		m = math.Round(f * 100 / float64(int(1)<<(e+1)))
		e++
	}

	raw := uint16(e)<<11 | uint16(int(m)&0x07FF)
	if m < 0 {
		raw |= 0x8000
	}

	return binary.BigEndian.AppendUint16(nil, raw), nil
}
