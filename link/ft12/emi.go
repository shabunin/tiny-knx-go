package ft12

import (
	"errors"
	"io"

	"github.com/shabunin/tiny-knx-go/cemi"
)

// ErrFrameTooLong is returned when an EMI2 module is asked to send an extended frame.
var ErrFrameTooLong = errors.New("ft12: EMI2 modules only send standard frames")

// ErrNotSupported is returned for property access on an EMI2 module, which has no interface
// objects.
var ErrNotSupported = errors.New("ft12: not supported by EMI2 modules")

// Protocol is the message format of a module: cEMI for current modules such as kBerry, EMI2 for
// older BCU2-based interfaces such as the first BAOS modules.
type Protocol uint32

const (
	// ProtocolAuto asks the module in cEMI and falls back to EMI2 if it does not answer.
	ProtocolAuto Protocol = iota
	ProtocolCEMI
	ProtocolEMI2
)

func (p Protocol) String() string {
	switch p {
	case ProtocolCEMI:
		return "cEMI"
	case ProtocolEMI2:
		return "EMI2"
	default:
		return "auto"
	}
}

// EMI2 message codes (KNX 03_06_03). L_Data uses the same codes as cEMI.
const (
	emiPCSetValueReq  = 0x46
	emiPEIIdentifyReq = 0xA7
	emiPEIIdentifyCon = 0xA8
	emiPEISwitchReq   = 0xA9
)

// PEI_Switch.req messages that route the layers of the module to the PEI.
var (
	emiSwitchLinkLayer = []byte{emiPEISwitchReq, 0x00, 0x18, 0x34, 0x56, 0x78, 0x0A}
	emiSwitchNormal    = []byte{emiPEISwitchReq, 0x1E, 0x12, 0x34, 0x56, 0x78, 0x9A}
)

// The individual address in the memory of a BCU2.
const emiAddressMemory = 0x0117

// An EMI2 L_Data message is a TP1 frame without the checksum:
//
//	code | control | source (2) | destination (2) | address type, hops, length | TPDU
//
// cEMI adds the additional info, a second control field and moves the length:
//
//	code | info length | info | control 1 | control 2 | source (2) | destination (2) | length | TPDU

// emiFromCEMI converts a cEMI L_Data message to EMI2.
func emiFromCEMI(frame []byte) ([]byte, error) {
	if len(frame) < 2 || len(frame) < 2+int(frame[1])+7 {
		return nil, io.ErrUnexpectedEOF
	}

	f := frame[2+frame[1]:] // without the additional info
	length := f[6]

	if length > 15 {
		return nil, ErrFrameTooLong
	}

	emi := make([]byte, 0, len(f))
	emi = append(emi,
		frame[0],
		f[0]&0x0F,        // priority, ack request; the module sets the rest
		0,                // source, filled in by the module
		0,                //
		f[4],             // destination
		f[5],             //
		f[1]&0xF0|length, // address type and hop count from control 2, length
	)

	return append(emi, f[7:]...), nil
}

// emiToCEMI converts an EMI2 L_Data message to cEMI, or reports false for other messages.
func emiToCEMI(emi []byte) ([]byte, bool) {
	switch cemi.MessageCode(emi[0]) {
	case cemi.LDataReqCode, cemi.LDataConCode, cemi.LDataIndCode:
	default:
		return nil, false
	}

	if len(emi) < 8 {
		return nil, false
	}

	frame := make([]byte, 0, len(emi)+2)
	frame = append(frame,
		emi[0],
		0,           // no additional info
		emi[1],      // the TP1 control field matches control 1
		emi[6]&0xF0, // address type, hop count
		emi[2],      // source
		emi[3],      //
		emi[4],      // destination
		emi[5],      //
		emi[6]&0x0F, // length
	)

	return append(frame, emi[7:]...), true
}
