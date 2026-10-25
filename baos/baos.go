// Package baos implements the ObjectServer protocol of Weinzierl BAOS devices (KNX BAOS binary
// protocol). In this mode the module is a KNX device of its own: ETS loads its application, which
// defines datapoints (group objects) and parameters, and a client reads and writes the datapoints
// instead of telegrams. In data link layer mode (package link/ft12) the module passes telegrams
// instead, and its own device stack does not answer on the bus.
//
// Two protocol versions exist. Version 1 (BAOS Module 820/822, Serial BAOS 870, IP BAOS 770)
// encodes IDs and counts in one byte; version 2 (BAOS Module 830/832/838 kBerry, USB interfaces,
// IP BAOS 771 and later) in two. The codec handles both; Client detects the version.
//
// Every message starts with the main service 0xF0 and a subservice. Requests have the subservice
// codes below; a response sets bit 7 (0x80) of its request's code; indications are 0xC1 and 0xC2.
// Over FT1.2 a message is the payload of a data frame, as cEMI messages are in data link layer
// mode.
package baos

import "strconv"

// MainService is the first byte of every ObjectServer message.
const MainService = 0xF0

// Version is the major version of the ObjectServer protocol.
type Version uint8

// These are the protocol versions.
const (
	Version1 Version = 1
	Version2 Version = 2
)

// These are the subservices of requests. Responses add ResponseFlag.
const (
	GetServerItem            = 0x01
	SetServerItem            = 0x02
	GetDatapointDescription  = 0x03
	GetDescriptionString     = 0x04
	GetDatapointValue        = 0x05
	SetDatapointValue        = 0x06
	GetParameterByte         = 0x07
	SetParameterByte         = 0x08 // version 2.2 and later
	GetDatapointDescription2 = 0x08 // version 1 only: descriptions with the datapoint type

	ResponseFlag = 0x80

	DatapointValueIndication = 0xC1
	ServerItemIndication     = 0xC2 // version 2 only
)

// These are the server items. Items 1 to 15 exist in both versions, the others in version 2. Values
// longer than one byte are big-endian.
const (
	ItemHardwareType         = 1  // 6 bytes, PID_HARDWARE_TYPE
	ItemHardwareVersion      = 2  // e.g. 0x10 = 1.0
	ItemFirmwareVersion      = 3  // e.g. 0x10 = 1.0
	ItemManufacturer         = 4  // manufacturer of the device
	ItemApplicationVendor    = 5  // manufacturer of the application loaded by ETS
	ItemApplicationID        = 6  // ID of the application loaded by ETS
	ItemApplicationVersion   = 7  // version of the application loaded by ETS
	ItemSerialNumber         = 8  // 6 bytes
	ItemTimeSinceReset       = 9  // milliseconds, 4 bytes
	ItemBusConnected         = 10 // 1 = connected
	ItemMaxBufferSize        = 11 // 2 bytes
	ItemDescriptionLength    = 12 // length of the description strings, 2 bytes
	ItemBaudRate             = 13 // 1 = 19200, 2 = 115200
	ItemBufferSize           = 14 // current buffer size, 2 bytes, writable
	ItemProgrammingMode      = 15 // bit 0, writable
	ItemProtocolVersion      = 16 // version 2: e.g. 0x20 = 2.0
	ItemIndicationSending    = 17 // version 2: bit 0, writable
	ItemIndividualAddress    = 20 // optional
	ItemMaxDatapoints        = 38 // optional
	ItemConfiguredDatapoints = 39 // optional
	ItemMaxParameterBytes    = 40 // optional
	ItemDownloadCounter      = 41 // optional
	ItemSupportedStatusFlags = 52 // optional
	ItemStatusFlags          = 53 // optional
)

// An Item is a server item with its value.
type Item struct {
	ID   uint16
	Data []byte
}

// ValueType is the size of a datapoint value.
type ValueType uint8

// These are the value types.
const (
	Bits1    ValueType = 0
	Bits2    ValueType = 1
	Bits3    ValueType = 2
	Bits4    ValueType = 3
	Bits5    ValueType = 4
	Bits6    ValueType = 5
	Bits7    ValueType = 6
	Bytes1   ValueType = 7
	Bytes2   ValueType = 8
	Bytes3   ValueType = 9
	Bytes4   ValueType = 10
	Bytes6   ValueType = 11
	Bytes8   ValueType = 12
	Bytes10  ValueType = 13
	Bytes14  ValueType = 14
	Variable ValueType = 15 // version 1
)

var valueSizes = [...]uint8{1, 1, 1, 1, 1, 1, 1, 1, 2, 3, 4, 6, 8, 10, 14}

// Size returns the size of a value of this type in bytes, or 0 if it is not fixed. Values shorter
// than a byte take the low bits of one byte.
func (t ValueType) Size() int {
	if int(t) < len(valueSizes) {
		return int(valueSizes[t])
	}

	return 0
}

// Bits returns the size of a value of this type in bits.
func (t ValueType) Bits() int {
	if t <= Bits7 {
		return int(t) + 1
	}

	return t.Size() * 8
}

// Flags are the configuration flags of a datapoint. The bits are the same in both versions, except
// that version 1 has no ReadOnInit.
type Flags uint8

// Priority returns the transmit priority, coded as in the control field of a telegram: 0 system,
// 1 high (normal), 2 alarm (urgent), 3 low.
func (f Flags) Priority() uint8 { return uint8(f & 0x03) }

// Communication reports whether the datapoint communicates at all (C flag).
func (f Flags) Communication() bool { return f&0x04 != 0 }

// Read reports whether the datapoint answers reads from the bus (R flag).
func (f Flags) Read() bool { return f&0x08 != 0 }

// Write reports whether the datapoint takes writes from the bus (W flag).
func (f Flags) Write() bool { return f&0x10 != 0 }

// ReadOnInit reports whether the datapoint reads its value after a reset (I flag, version 2).
func (f Flags) ReadOnInit() bool { return f&0x20 != 0 }

// Transmit reports whether the datapoint sends to the bus (T flag).
func (f Flags) Transmit() bool { return f&0x40 != 0 }

// UpdateOnResponse reports whether responses update the value (U flag).
func (f Flags) UpdateOnResponse() bool { return f&0x80 != 0 }

// String returns the flags as ETS shows them, e.g. "CRWTU", with dashes for flags that are off.
func (f Flags) String() string {
	b := []byte("CRWTUI")
	for i, on := range []bool{f.Communication(), f.Read(), f.Write(), f.Transmit(), f.UpdateOnResponse(), f.ReadOnInit()} {
		if !on {
			b[i] = '-'
		}
	}

	return string(b)
}

// DPT is the datapoint type code of a description. Codes 1 to 19 are the main numbers of the KNX
// datapoint types; 32 is DPT 20, 33 is DPT 232, 34 is DPT 251.
type DPT uint8

// These are the datapoint type codes that are not a DPT main number.
const (
	DPTDisabled DPT = 0
	DPT20       DPT = 32
	DPT232      DPT = 33
	DPT251      DPT = 34
	DPTUnknown  DPT = 255
)

// Main returns the main number of the KNX datapoint type, or 0 if it is not known.
func (t DPT) Main() int {
	switch {
	case t >= 1 && t <= 19:
		return int(t)
	case t == DPT20:
		return 20
	case t == DPT232:
		return 232
	case t == DPT251:
		return 251
	}

	return 0
}

// A Description describes a datapoint configured by ETS.
type Description struct {
	ID        uint16
	ValueType ValueType
	Flags     Flags
	DPT       DPT // DPTDisabled for unconfigured datapoints; 0 for version 1 without DPT
}

// TransmissionStatus is the state of sending a datapoint to the bus.
type TransmissionStatus uint8

// These are the transmission states.
const (
	TransmissionIdle       TransmissionStatus = 0
	TransmissionError      TransmissionStatus = 1
	TransmissionInProgress TransmissionStatus = 2
	TransmissionRequest    TransmissionStatus = 3
)

// State is the state of a datapoint value.
type State struct {
	Valid        bool // the value is known (always true in version 1, which reports unknown values as errors)
	Updated      bool // the value was updated from the bus
	ReadRequest  bool // a GroupValueRead is pending
	Transmission TransmissionStatus
}

// A Value is the value of a datapoint.
type Value struct {
	ID    uint16
	State State
	Data  []byte
}

// Command tells the server what to do with a datapoint in SetDatapointValue.
type Command uint8

// These are the datapoint commands.
const (
	CommandNone              Command = 0
	CommandSet               Command = 1 // set the value
	CommandSend              Command = 2 // send the current value to the bus
	CommandSetAndSend        Command = 3
	CommandRead              Command = 4 // send a GroupValueRead
	CommandClearTransmission Command = 5
)

// A SetValue is a datapoint command, with the new value for CommandSet and CommandSetAndSend.
type SetValue struct {
	ID      uint16
	Command Command
	Data    []byte
}

// Filter selects datapoint values in GetDatapointValue (version 2).
type Filter uint8

// These are the value filters.
const (
	FilterAll     Filter = 0
	FilterValid   Filter = 1
	FilterUpdated Filter = 2
)

// ErrorCode is the error of a negative response.
type ErrorCode uint8

// These are the error codes.
const (
	ErrInternal            ErrorCode = 1
	ErrNoElement           ErrorCode = 2 // item missing, datapoint not configured or without value
	ErrBufferTooSmall      ErrorCode = 3
	ErrNotWritable         ErrorCode = 4
	ErrServiceNotSupported ErrorCode = 5
	ErrBadParameter        ErrorCode = 6 // ID out of range
	ErrBadID               ErrorCode = 7
	ErrBadCommand          ErrorCode = 8
	ErrBadLength           ErrorCode = 9
	ErrInconsistent        ErrorCode = 10
	ErrBusy                ErrorCode = 11
)

var errorText = [...]string{
	ErrInternal:            "internal error",
	ErrNoElement:           "no element found",
	ErrBufferTooSmall:      "buffer too small",
	ErrNotWritable:         "item not writable",
	ErrServiceNotSupported: "service not supported",
	ErrBadParameter:        "bad service parameter",
	ErrBadID:               "bad ID",
	ErrBadCommand:          "bad command or value",
	ErrBadLength:           "bad length",
	ErrInconsistent:        "message inconsistent",
	ErrBusy:                "object server busy",
}

func (e ErrorCode) Error() string {
	if int(e) < len(errorText) && errorText[e] != "" {
		return "baos: " + errorText[e]
	}

	return "baos: error " + strconv.Itoa(int(e))
}
