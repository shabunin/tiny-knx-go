//go:build tinygo

// Command pico-kberry is a minimal KNX switch actuator: a Raspberry Pi Pico with a kBerry module
// on UART0 (GP0 = TX to kBerry RX, GP1 = RX from kBerry TX, plus 3V3 and GND). The on-board LED
// follows group address 1/1/1 and its state is reported on 1/1/2. Telegrams are logged to USB.
//
//	tinygo flash -target=pico ./examples/pico-kberry
package main

import (
	"machine"
	"time"

	"github.com/shabunin/tiny-knx-go/cemi"
	"github.com/shabunin/tiny-knx-go/link/ft12"
)

var (
	switchAddr = cemi.NewGroupAddr3(1, 1, 1)
	statusAddr = cemi.NewGroupAddr3(1, 1, 2)
)

func main() {
	machine.LED.Configure(machine.PinConfig{Mode: machine.PinOutput})

	uart := machine.UART0
	uart.Configure(machine.UARTConfig{
		BaudRate: 19200,
		TX:       machine.UART0_TX_PIN,
		RX:       machine.UART0_RX_PIN,
	})
	uart.SetFormat(8, 1, machine.ParityEven)

	var l *ft12.Link
	for {
		var err error
		if l, err = ft12.Open(uart, ft12.DefaultConfig); err == nil {
			break
		}

		println("kBerry not responding:", err.Error())
		time.Sleep(2 * time.Second)
	}

	println("kBerry ready")

	for ind := range l.Inbound() {
		app, ok := ind.Data.(*cemi.AppData)
		if !ok || !ind.Control2.IsGroupAddr() {
			continue
		}

		dest := cemi.GroupAddr(ind.Destination)

		switch {
		case app.Command == cemi.GroupValueWrite && dest == switchAddr:
			on := len(app.Data) > 0 && app.Data[0]&1 == 1
			machine.LED.Set(on)
			println("switch:", on, "from", ind.Source.String())

			report(l, statusAddr, cemi.GroupValueWrite, on)

		case app.Command == cemi.GroupValueRead && (dest == switchAddr || dest == statusAddr):
			report(l, dest, cemi.GroupValueResponse, machine.LED.Get())
		}
	}
}

// report sends the LED state to a group address.
func report(l *ft12.Link, dest cemi.GroupAddr, command cemi.APCI, on bool) {
	var value byte
	if on {
		value = 1
	}

	err := l.Send(&cemi.LDataReq{LData: cemi.LData{
		Control1:    cemi.Control1Request(cemi.PrioLow),
		Control2:    cemi.Control2GroupAddr | cemi.Control2Hops(6),
		Destination: uint16(dest),
		Data:        &cemi.AppData{Command: command, Data: []byte{value}},
	}})
	if err != nil {
		println("status not sent:", err.Error())
	}
}
