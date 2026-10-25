package cemi

import (
	"io"

	"github.com/shabunin/tiny-knx-go/internal/util"
)

// Interface object types and property IDs that are needed to manage a cEMI server, e.g. to switch a
// KNX BAOS module (kBerry) into data link layer mode.
const (
	ObjectTypeDevice     uint16 = 0
	ObjectTypeCEMIServer uint16 = 8

	PIDSubnetAddr       uint8 = 57 // in the device object: high byte of the individual address
	PIDDeviceAddr       uint8 = 58 // in the device object: low byte of the individual address
	PIDDeviceDescriptor uint8 = 83 // in the device object
	PIDSerialNumber     uint8 = 11 // in the device object
	PIDCommMode         uint8 = 52 // in the cEMI server object

	CommModeDataLinkLayer  uint8 = 0x00
	CommModeBusmonitor     uint8 = 0x01
	CommModeRawFrames      uint8 = 0x02
	CommModeNoLayer        uint8 = 0xFF
	CommModeTransportLayer uint8 = 0x06
	CommModeObjectServer   uint8 = 0xF0 // BAOS mode of Weinzierl modules (package baos)
)

// MProp is the body of the M_Prop* messages, which access the interface object properties of a
// cEMI server.
type MProp struct {
	ObjectType uint16
	Instance   uint8
	PropertyID uint8

	// Count is the number of elements (4 bits). A confirmation with Count 0 reports an error, and
	// Data then holds the error code.
	Count uint8

	// Start is the start index (12 bits).
	Start uint16

	Data []byte
}

// Failed reports whether this is a negative confirmation.
func (prop *MProp) Failed() bool {
	return prop.Count == 0
}

// Size returns the packed size.
func (prop *MProp) Size() uint {
	return 6 + uint(len(prop.Data))
}

// Pack the message body into the buffer.
func (prop *MProp) Pack(buffer []byte) {
	util.PackSome(
		buffer,
		prop.ObjectType,
		prop.Instance,
		prop.PropertyID,
		uint16(prop.Count&15)<<12|prop.Start&0xFFF,
		prop.Data,
	)
}

// Unpack initializes the structure by parsing the given data.
func (prop *MProp) Unpack(data []byte) (n uint, err error) {
	if len(data) < 6 {
		return 0, io.ErrUnexpectedEOF
	}

	var countStart uint16

	n, err = util.UnpackSome(data, &prop.ObjectType, &prop.Instance, &prop.PropertyID, &countStart)
	if err != nil {
		return
	}

	prop.Count = uint8(countStart >> 12)
	prop.Start = countStart & 0xFFF
	prop.Data = make([]byte, len(data)-6)
	n += uint(copy(prop.Data, data[6:]))

	return
}

// A MPropReadReq represents a M_PropRead.req message body.
type MPropReadReq struct {
	MProp
}

// MessageCode returns the message code for M_PropRead.req.
func (MPropReadReq) MessageCode() MessageCode {
	return MPropReadReqCode
}

// A MPropReadCon represents a M_PropRead.con message body.
type MPropReadCon struct {
	MProp
}

// MessageCode returns the message code for M_PropRead.con.
func (MPropReadCon) MessageCode() MessageCode {
	return MPropReadConCode
}

// A MPropWriteReq represents a M_PropWrite.req message body.
type MPropWriteReq struct {
	MProp
}

// MessageCode returns the message code for M_PropWrite.req.
func (MPropWriteReq) MessageCode() MessageCode {
	return MPropWriteReqCode
}

// A MPropWriteCon represents a M_PropWrite.con message body.
type MPropWriteCon struct {
	MProp
}

// MessageCode returns the message code for M_PropWrite.con.
func (MPropWriteCon) MessageCode() MessageCode {
	return MPropWriteConCode
}

// A MPropInfoInd represents a M_PropInfo.ind message body.
type MPropInfoInd struct {
	MProp
}

// MessageCode returns the message code for M_PropInfo.ind.
func (MPropInfoInd) MessageCode() MessageCode {
	return MPropInfoIndCode
}

// MReset is the empty body of the M_Reset messages.
type MReset struct{}

// Size returns the packed size.
func (MReset) Size() uint {
	return 0
}

// Pack the message body into the buffer.
func (MReset) Pack([]byte) {}

// Unpack initializes the structure by parsing the given data.
func (*MReset) Unpack([]byte) (uint, error) {
	return 0, nil
}

// A MResetReq represents a M_Reset.req message body.
type MResetReq struct {
	MReset
}

// MessageCode returns the message code for M_Reset.req.
func (MResetReq) MessageCode() MessageCode {
	return MResetReqCode
}

// A MResetInd represents a M_Reset.ind message body.
type MResetInd struct {
	MReset
}

// MessageCode returns the message code for M_Reset.ind.
func (MResetInd) MessageCode() MessageCode {
	return MResetIndCode
}
