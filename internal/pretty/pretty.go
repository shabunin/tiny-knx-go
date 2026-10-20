// Package pretty formats frames for the command line tools.
package pretty

import (
	"fmt"
	"strings"

	"github.com/shabunin/tiny-knx-go/apdu"
	"github.com/shabunin/tiny-knx-go/cemi"
)

// Frame describes a frame in one line: source, destination and the decoded transport unit.
func Frame(ldata *cemi.LData) string {
	var dest string
	if ldata.Control2.IsGroupAddr() {
		dest = cemi.GroupAddr(ldata.Destination).String()
	} else {
		dest = cemi.IndividualAddr(ldata.Destination).String()
	}

	line := fmt.Sprintf("%-9s -> %-9s ", ldata.Source, dest)

	switch tpdu := ldata.Data.(type) {
	case *cemi.ControlData:
		names := [4]string{"T_Connect", "T_Disconnect", "T_ACK", "T_NAK"}
		line += names[tpdu.Command&3]

		if tpdu.Numbered {
			line += fmt.Sprintf(" seq=%d", tpdu.SeqNumber)
		}

	case *cemi.AppData:
		// Group services and unknown ones are shown with their raw data, the rest decoded.
		a, err := apdu.Decode(tpdu)
		raw := err != nil

		switch a.(type) {
		case apdu.GroupValueRead, apdu.GroupValueResponse, apdu.GroupValueWrite, apdu.Unknown:
			raw = true
		}

		if raw {
			line += APCIName(tpdu.Command)
		} else {
			line += strings.TrimPrefix(fmt.Sprintf("%T", a), "apdu.")
		}

		if tpdu.Numbered {
			line += fmt.Sprintf(" seq=%d", tpdu.SeqNumber)
		}

		if raw {
			line += fmt.Sprintf(" % x", tpdu.Data)
		} else {
			line += fmt.Sprintf(" %+v", a)
		}
	}

	return line
}

// APCIName returns the name of an application layer service.
func APCIName(command cemi.APCI) string {
	names := [16]string{
		"GroupValueRead", "GroupValueResponse", "GroupValueWrite", "IndividualAddrWrite",
		"IndividualAddrRead", "IndividualAddrResponse", "AdcRead", "AdcResponse",
		"MemoryRead", "MemoryResponse", "MemoryWrite", "UserMessage",
		"DeviceDescriptorRead", "DeviceDescriptorResponse", "Restart", "Escape",
	}

	return names[command&15]
}
