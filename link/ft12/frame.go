// Package ft12 implements the FT1.2 serial protocol (IEC 60870-5-2) used by KNX BAOS modules such as
// the Weinzierl kBerry (BAOS 838), and a link.Link that exchanges cEMI frames through it.
package ft12

// Frame layout:
//
//	ack:      E5
//	fixed:    10 C CS 16                 (CS = C)
//	variable: 68 L L 68 C payload… CS 16 (L = 1 + len(payload), CS = C + sum(payload))
const (
	startFixed    = 0x10
	startVariable = 0x68
	frameEnd      = 0x16

	// Ack acknowledges a received frame.
	Ack = 0xE5

	// maxPayload is the largest payload that fits the length byte.
	maxPayload = 254
)

// Control field values. Bit 5 is the frame count bit (FCB), which alternates between consecutive
// data frames so that the receiver can recognise retransmissions.
const (
	ControlResetReq   = 0x40 // host → module, fixed frame
	ControlResetInd   = 0xC0 // module → host, fixed frame
	ControlHostData   = 0x53 // host → module, variable frame, FCB = 0
	ControlModuleData = 0xD3 // module → host, variable frame, FCB = 0
	ControlFCB        = 0x20
)

// FrameKind tells what kind of FT1.2 frame was decoded.
type FrameKind uint8

// These are the FT1.2 frame kinds.
const (
	KindAck FrameKind = iota
	KindFixed
	KindVariable
)

// A Frame is a decoded FT1.2 frame.
type Frame struct {
	Kind    FrameKind
	Control byte
	Payload []byte
}

// AppendVariable appends a variable length frame to dst.
func AppendVariable(dst []byte, control byte, payload []byte) []byte {
	length := byte(len(payload) + 1)
	sum := control

	dst = append(dst, startVariable, length, length, startVariable, control)
	for _, b := range payload {
		sum += b
	}

	dst = append(dst, payload...)

	return append(dst, sum, frameEnd)
}

// AppendFixed appends a fixed length frame to dst.
func AppendFixed(dst []byte, control byte) []byte {
	return append(dst, startFixed, control, control, frameEnd)
}

// A Decoder extracts frames from a byte stream. Data may arrive in arbitrary chunks; bytes that
// cannot start a valid frame are skipped.
type Decoder struct {
	buf []byte
}

// Write appends received bytes to the decoder.
func (d *Decoder) Write(data []byte) (int, error) {
	d.buf = append(d.buf, data...)
	return len(data), nil
}

// Next returns the next complete frame, or false if more data is needed. The returned payload is
// a copy that stays valid after further calls.
func (d *Decoder) Next() (Frame, bool) {
	for len(d.buf) > 0 {
		switch d.buf[0] {
		case Ack:
			d.consume(1)
			return Frame{Kind: KindAck}, true

		case startFixed:
			if len(d.buf) < 4 {
				return Frame{}, false
			}

			if d.buf[1] == d.buf[2] && d.buf[3] == frameEnd {
				frame := Frame{Kind: KindFixed, Control: d.buf[1]}
				d.consume(4)

				return frame, true
			}

		case startVariable:
			if len(d.buf) < 4 {
				return Frame{}, false
			}

			length := int(d.buf[1])
			if length > 0 && d.buf[2] == d.buf[1] && d.buf[3] == startVariable {
				total := 4 + length + 2
				if len(d.buf) < total {
					return Frame{}, false
				}

				var sum byte
				for _, b := range d.buf[4 : 4+length] {
					sum += b
				}

				if d.buf[4+length] == sum && d.buf[total-1] == frameEnd {
					frame := Frame{
						Kind:    KindVariable,
						Control: d.buf[4],
						Payload: append([]byte(nil), d.buf[5:4+length]...),
					}
					d.consume(total)

					return frame, true
				}
			}
		}

		// Not the start of a valid frame: resynchronise on the next byte.
		d.consume(1)
	}

	return Frame{}, false
}

func (d *Decoder) consume(n int) {
	remaining := copy(d.buf, d.buf[n:])
	d.buf = d.buf[:remaining]
}
