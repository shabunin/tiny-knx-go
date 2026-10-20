//go:build tinygo

// Command pico-kberry is a minimal KNX switch actuator: a Raspberry Pi Pico with a kBerry module
// on UART0 (GP0 = TX to kBerry RX, GP1 = RX from kBerry TX, plus 3V3 and GND). The on-board LED
// follows group address 1/1/1 and its state is reported on 1/1/2. Events are logged to USB.
//
// A button from GP15 to GND switches programming mode, shown by an LED on GP14, so that ETS can
// assign an individual address. The address is not saved: after a power cycle the device is
// 15.15.255 again.
//
//	tinygo flash -target=pico ./examples/pico-kberry
package main

import (
	"machine"
	"time"

	"github.com/shabunin/tiny-knx-go/apdu"
	"github.com/shabunin/tiny-knx-go/cemi"
	"github.com/shabunin/tiny-knx-go/device"
	"github.com/shabunin/tiny-knx-go/dpt"
	"github.com/shabunin/tiny-knx-go/link/ft12"
)

const (
	progButton = machine.GP15
	progLED    = machine.GP14
)

var config = device.Config{
	Objects: []device.ObjectConfig{
		{
			Name:      "switch",
			DPT:       "1.001",
			Flags:     device.FlagCommunicate | device.FlagWrite,
			Addresses: []cemi.GroupAddr{cemi.NewGroupAddr3(1, 1, 1)},
		},
		{
			Name:      "status",
			DPT:       "1.001",
			Flags:     device.FlagCommunicate | device.FlagRead | device.FlagTransmit,
			Addresses: []cemi.GroupAddr{cemi.NewGroupAddr3(1, 1, 2)},
		},
	},
}

func main() {
	machine.LED.Configure(machine.PinConfig{Mode: machine.PinOutput})
	progLED.Configure(machine.PinConfig{Mode: machine.PinOutput})
	progButton.Configure(machine.PinConfig{Mode: machine.PinInputPullup})

	identity := device.Identity{
		OrderNumber:      "PICO-KBERRY",
		FirmwareRevision: 1,
	}

	// Manufacturer 0x00FA (KNX Association, used by community projects) and the board's unique ID.
	identity.SerialNumber = apdu.SerialNumber{0x00, 0xFA}
	id := machine.DeviceID()
	copy(identity.SerialNumber[2:], id[len(id)-4:])

	d, err := device.New(identity, config)
	if err != nil {
		panic(err)
	}

	status := d.Object("status")

	d.Object("switch").OnChange(func(o *device.Object) {
		var on dpt.DPT_1001
		o.Get(&on)

		machine.LED.Set(bool(on))
		println("switch:", bool(on))

		status.Set(on)
	})

	d.OnProgModeChange(func(on bool) {
		progLED.Set(on)
		println("programming mode:", on)
	})

	d.OnAddressChange(func(addr cemi.IndividualAddr) {
		println("new address:", addr.String())
	})

	go watchButton(d)

	uart := machine.UART0
	uart.Configure(machine.UARTConfig{
		BaudRate: 19200,
		TX:       machine.UART0_TX_PIN,
		RX:       machine.UART0_RX_PIN,
	})
	uart.SetFormat(8, 1, machine.ParityEven)

	for {
		l, err := ft12.Open(uart, ft12.DefaultConfig)
		if err != nil {
			println("kBerry not responding:", err.Error())
			time.Sleep(2 * time.Second)

			continue
		}

		println("kBerry ready, address", d.Address().String())

		err = d.Run(l)
		println("device stopped:", err.Error())
		l.Close()
		time.Sleep(2 * time.Second)
	}
}

// watchButton toggles programming mode when the button is pressed.
func watchButton(d *device.Device) {
	wasPressed := false

	for {
		pressed := !progButton.Get()
		if pressed && !wasPressed {
			d.SetProgMode(!d.ProgMode())
		}

		wasPressed = pressed

		time.Sleep(20 * time.Millisecond) // debounce
	}
}
