package device

import (
	"fmt"
	"strings"

	"github.com/shabunin/tiny-knx-go/apdu"
	"github.com/shabunin/tiny-knx-go/cemi"
)

// DefaultAddress is the individual address of a device that has not been programmed.
var DefaultAddress = cemi.NewIndividualAddr3(15, 15, 255)

// Flags control how a group object communicates.
type Flags uint8

// These are the group object flags.
const (
	FlagCommunicate Flags = 1 << iota // C: the object communicates at all
	FlagRead                          // R: answer GroupValueRead with the value
	FlagWrite                         // W: accept GroupValueWrite
	FlagTransmit                      // T: send the value when the application sets it
	FlagUpdate                        // U: accept GroupValueResponse as a new value
	FlagInit                          // I: read the value from the bus when the device starts
)

const flagLetters = "CRWTUI"

// String returns the flags as letters, e.g. "CWT".
func (f Flags) String() string {
	var b strings.Builder

	for i := range flagLetters {
		if f&(1<<i) != 0 {
			b.WriteByte(flagLetters[i])
		}
	}

	return b.String()
}

// MarshalText implements encoding.TextMarshaler.
func (f Flags) MarshalText() ([]byte, error) {
	return []byte(f.String()), nil
}

// UnmarshalText parses flags from letters, e.g. "CWT" or "c-w-t".
func (f *Flags) UnmarshalText(text []byte) error {
	var flags Flags

	for _, c := range strings.ToUpper(string(text)) {
		if c == '-' || c == ' ' {
			continue
		}

		i := strings.IndexRune(flagLetters, c)
		if i < 0 {
			return fmt.Errorf("device: unknown flag %q", c)
		}

		flags |= 1 << i
	}

	*f = flags

	return nil
}

// ObjectConfig configures a group object.
type ObjectConfig struct {
	Name string `json:"name,omitempty"`

	// DPT is the datapoint type, e.g. "1.001". It determines the initial value.
	DPT string `json:"dpt,omitempty"`

	Flags Flags `json:"flags"`

	// Addresses are the group addresses linked to the object. The first one is used for sending.
	Addresses []cemi.GroupAddr `json:"addresses,omitempty"`
}

// Config is the part of a device's setup that changes per installation: its address and the
// links between its group objects and group addresses. It is plain data, so it can come from
// code, a JSON file, a web page or an ETS download.
type Config struct {
	Address cemi.IndividualAddr `json:"address"`
	Objects []ObjectConfig      `json:"objects"`
}

// Identity describes the device itself. It is fixed by the firmware.
type Identity struct {
	// MaskVersion is reported as device descriptor type 0, e.g. 0x07B0 for System B.
	MaskVersion uint16

	// SerialNumber must be unique. Its first two bytes are the manufacturer ID.
	SerialNumber apdu.SerialNumber

	// OrderNumber is the product's order number, at most 10 characters.
	OrderNumber string

	HardwareType     [6]byte
	FirmwareRevision uint8
}

// DefaultMaskVersion is reported when Identity.MaskVersion is zero.
const DefaultMaskVersion = 0x07B0
