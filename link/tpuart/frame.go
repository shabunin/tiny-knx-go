package tpuart

import (
	"errors"

	"github.com/shabunin/tiny-knx-go/cemi"
)

var (
	// ErrInvalidFrame is returned for TP1 frames that are truncated or whose length field does not
	// match.
	ErrInvalidFrame = errors.New("tpuart: invalid frame")

	// ErrChecksum is returned for TP1 frames with a wrong checksum.
	ErrChecksum = errors.New("tpuart: frame checksum mismatch")

	// ErrFrameTooLong is returned for frames whose TPDU does not fit into an extended frame.
	ErrFrameTooLong = errors.New("tpuart: frame too long")
)

// TP1 frame layout. A standard frame carries up to 15 bytes after the TPCI:
//
//	ctrl | source (2) | destination (2) | AT hops length | TPCI | APCI/data... | checksum
//
// An extended frame carries up to 254:
//
//	ctrl | ctrlE (AT hops EFF) | source (2) | destination (2) | length | TPCI | ... | checksum
//
// The control byte is "F0R1PP00": F = 1 for standard frames, R = 0 for repetitions, PP the
// priority. cEMI uses the same bits in its first control field, and the cEMI length field equals
// the TP1 length field.
const (
	ctrlStandard   = 0x80
	ctrlNotRepeat  = 0x20
	ctrlFixed      = 0x10 // always set on TP1
	ctrlStartMask  = 0xD3
	ctrlStdStart   = 0x90 // ctrl&ctrlStartMask of a standard frame
	ctrlExtStart   = 0x10 // ctrl&ctrlStartMask of an extended frame
	maxStdLength   = 15
	maxExtLength   = 254 // 255 is reserved as escape
	stdHeaderSize  = 7   // up to and including the TPCI
	extHeaderSize  = 8
	maxFrameLength = extHeaderSize + maxExtLength + 1
)

// Checksum returns the TP1 checksum of data: the inverted XOR of all bytes.
func Checksum(data []byte) byte {
	var sum byte
	for _, b := range data {
		sum ^= b
	}

	return ^sum
}

// isStandard tells whether a frame starting with ctrl is a standard frame.
func isStandard(ctrl byte) bool {
	return ctrl&ctrlStandard != 0
}

// isFrameStart tells whether b is the control byte of an L_Data frame.
func isFrameStart(b byte) bool {
	return b&ctrlStartMask == ctrlStdStart || b&ctrlStartMask == ctrlExtStart
}

// frameSize returns the total size of a frame, including the checksum, from its first bytes. It
// needs 6 bytes of a standard frame and 7 of an extended frame.
func frameSize(head []byte) (int, bool) {
	if len(head) < 6 {
		return 0, false
	}

	if isStandard(head[0]) {
		return stdHeaderSize + int(head[5]&0x0F) + 1, true
	}

	if len(head) < 7 {
		return 0, false
	}

	return extHeaderSize + int(head[6]) + 1, true
}

// frameAddresses returns the source, destination and address type of a frame from its first 6
// bytes.
func frameAddresses(head []byte) (src cemi.IndividualAddr, dest uint16, group bool) {
	if isStandard(head[0]) {
		return cemi.IndividualAddr(head[1])<<8 | cemi.IndividualAddr(head[2]),
			uint16(head[3])<<8 | uint16(head[4]), head[5]&0x80 != 0
	}

	return cemi.IndividualAddr(head[2])<<8 | cemi.IndividualAddr(head[3]),
		uint16(head[4])<<8 | uint16(head[5]), head[1]&0x80 != 0
}

// AppendFrame appends the TP1 frame for ldata, including the checksum, to buf. A standard frame is
// used unless the TPDU is too long for one or Control1 asks for an extended frame. The repeat flag
// is cleared (first transmission) and the bits that cEMI uses for acknowledgement requests and
// confirmations are dropped.
func AppendFrame(buf []byte, ldata *cemi.LData) ([]byte, error) {
	if ldata.Data == nil {
		return buf, ErrInvalidFrame
	}

	// The packed TPDU starts with the cEMI length field, followed by TPCI and APCI/data.
	tpdu := make([]byte, ldata.Data.Size())
	ldata.Data.Pack(tpdu)

	length := len(tpdu) - 2
	if length > maxExtLength {
		return buf, ErrFrameTooLong
	}

	ctrl := byte(ldata.Control1)&0x0C | ctrlNotRepeat | ctrlFixed
	standard := length <= maxStdLength && ldata.Control1&cemi.Control1StdFrame != 0
	start := len(buf)

	src, dest := uint16(ldata.Source), ldata.Destination

	if standard {
		buf = append(buf,
			ctrl|ctrlStandard,
			byte(src>>8), byte(src), byte(dest>>8), byte(dest),
			byte(ldata.Control2)&0xF0|byte(length),
		)
	} else {
		buf = append(buf,
			ctrl,
			byte(ldata.Control2),
			byte(src>>8), byte(src), byte(dest>>8), byte(dest),
			byte(length),
		)
	}

	buf = append(buf, tpdu[1:]...)

	return append(buf, Checksum(buf[start:])), nil
}

// ParseFrame decodes a complete TP1 frame, including its checksum.
func ParseFrame(frame []byte) (cemi.LData, error) {
	size, ok := frameSize(frame)
	if !ok || len(frame) != size {
		return cemi.LData{}, ErrInvalidFrame
	}

	if Checksum(frame[:size-1]) != frame[size-1] {
		return cemi.LData{}, ErrChecksum
	}

	// Rebuild the cEMI layout (empty additional info, ctrl1, ctrl2, addresses, length, TPDU) and
	// let package cemi decode it.
	body := make([]byte, 0, size+2)
	body = append(body, 0, frame[0])

	if isStandard(frame[0]) {
		body = append(body, frame[5]&0xF0)
		body = append(body, frame[1:5]...)
		body = append(body, frame[5]&0x0F)
		body = append(body, frame[6:size-1]...)
	} else {
		body = append(body, frame[1:size-1]...)
	}

	var ldata cemi.LData
	if _, err := ldata.Unpack(body); err != nil {
		return cemi.LData{}, ErrInvalidFrame
	}

	ldata.Info = nil

	return ldata, nil
}
