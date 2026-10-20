//go:build linux && !tinygo

// Command knxmon talks to a KNX bus through a BAOS module (e.g. kBerry on a Raspberry Pi): it
// prints every telegram and can send group reads and writes.
//
//	knxmon -port /dev/serial0
//	knxmon -port /dev/serial0 -write 1/1/1=1     # switch on (DPT 1)
//	knxmon -port /dev/serial0 -write 1/1/2=0c1a  # 2 bytes, e.g. DPT 9 = 21.0 °C
//	knxmon -port /dev/serial0 -read 1/1/1
package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/shabunin/tiny-knx-go/cemi"
	"github.com/shabunin/tiny-knx-go/link/ft12"
	"github.com/shabunin/tiny-knx-go/serial"
)

func main() {
	port := flag.String("port", "/dev/serial0", "serial port of the BAOS module")
	baud := flag.Int("baud", 19200, "baud rate")
	write := flag.String("write", "", "send a group write: GA=HEX, e.g. 1/1/1=1")
	read := flag.String("read", "", "send a group read to GA, e.g. 1/1/1")
	flag.Parse()

	file, err := serial.Open(*port, *baud, serial.ParityEven)
	if err != nil {
		log.Fatal(err)
	}

	l, err := ft12.Open(file, ft12.DefaultConfig)
	if err != nil {
		log.Fatalf("Opening BAOS link on %s: %v", *port, err)
	}
	defer l.Close()

	if descriptor, err := l.ReadProperty(cemi.ObjectTypeDevice, 1, cemi.PIDDeviceDescriptor, 1, 1); err == nil {
		log.Printf("Module ready, device descriptor %x", descriptor)
	} else {
		log.Printf("Module ready, device descriptor unavailable: %v", err)
	}

	if *write != "" {
		addr, value, ok := strings.Cut(*write, "=")
		if !ok {
			log.Fatal("-write expects GA=HEX")
		}

		if len(value)%2 == 1 {
			value = "0" + value
		}

		data, err := hex.DecodeString(value)
		if err != nil || len(data) == 0 {
			log.Fatalf("Bad value %q: want hex bytes", value)
		}

		// Values up to 6 bits are packed into the APCI byte; longer ones follow it.
		if len(data) > 1 || data[0] > 0x3F {
			data = append([]byte{0}, data...)
		}

		send(l, addr, cemi.GroupValueWrite, data)
	}

	if *read != "" {
		send(l, *read, cemi.GroupValueRead, nil)
	}

	for ind := range l.Inbound() {
		fmt.Println(time.Now().Format("15:04:05.000"), describe(&ind.LData))
	}

	if dropped := l.Dropped(); dropped > 0 {
		log.Printf("%d frames were dropped", dropped)
	}

	os.Exit(1)
}

func send(l *ft12.Link, addr string, command cemi.APCI, data []byte) {
	ga, err := cemi.NewGroupAddrString(addr)
	if err != nil {
		log.Fatalf("Bad group address %q: %v", addr, err)
	}

	req := &cemi.LDataReq{LData: cemi.LData{
		Control1:    cemi.Control1StdFrame | cemi.Control1Prio(cemi.PrioLow),
		Control2:    cemi.Control2GroupAddr | cemi.Control2Hops(6),
		Destination: uint16(ga),
		Data:        &cemi.AppData{Command: command, Data: data},
	}}

	if err := l.Send(req); err != nil {
		log.Fatalf("Sending to %s: %v", ga, err)
	}

	log.Printf("Sent %s to %s", apciName(command), ga)
}

func describe(ldata *cemi.LData) string {
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
		line += apciName(tpdu.Command)

		if tpdu.Numbered {
			line += fmt.Sprintf(" seq=%d", tpdu.SeqNumber)
		}

		line += fmt.Sprintf(" % x", tpdu.Data)
	}

	return line
}

func apciName(command cemi.APCI) string {
	names := [16]string{
		"GroupValueRead", "GroupValueResponse", "GroupValueWrite", "IndividualAddrWrite",
		"IndividualAddrRead", "IndividualAddrResponse", "AdcRead", "AdcResponse",
		"MemoryRead", "MemoryResponse", "MemoryWrite", "UserMessage",
		"DeviceDescriptorRead", "DeviceDescriptorResponse", "Restart", "Escape",
	}

	return names[command&15]
}
