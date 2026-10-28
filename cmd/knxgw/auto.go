//go:build linux && !tinygo

package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/shabunin/tiny-knx-go/baos"
	"github.com/shabunin/tiny-knx-go/cemi"
	"github.com/shabunin/tiny-knx-go/internal/gateway"
	"github.com/shabunin/tiny-knx-go/link/ft12"
)

// runAuto serves a BAOS module that switches between its modes: in ObjectServer mode it serves the
// baos outputs, and while a tunnel of a knxip output is open, it is in data link layer mode.
func runAuto(input *gateway.Spec, outputs specs, trace bool) {
	port := input.Target

	baud, err := input.Int("baud", 19200)
	check(err)

	idle, err := input.Duration("idle", 5*time.Second)
	check(err)
	check(input.Unused())

	// The tunnels have the module's address, which data link layer mode tells.
	module, err := gateway.OpenModule(port, baud)
	check(err)

	addr, protocol := module.Address(), module.Protocol()
	module.Close()

	log.Printf("BAOS module on %s: %v at %v, in ObjectServer mode while no tunnel is open", port, protocol, addr)

	// cEMI modules (kBerry, Serial BAOS 870) speak version 2 of the ObjectServer protocol, EMI2
	// modules (BAOS 820) version 1.
	version := objectServerVersion(baos.Version1)
	if protocol == ft12.ProtocolCEMI {
		version = objectServerVersion(baos.Version2)
	}

	a := gateway.NewAutoModule(gateway.AutoConfig{
		OpenLink: func() (gateway.Uplink, error) {
			m, err := gateway.OpenModule(port, baud)
			if err == nil && m.Address() != addr {
				log.Printf("The module has the address %v now; the tunnels keep %v until knxgw restarts", m.Address(), addr)
			}

			return m, err
		},
		OpenBAOS: func() (*baos.Client, io.Closer, error) { return gateway.OpenBAOSPort(port, baud) },
		Idle:     idle,
		Retry:    3 * time.Second,
		Opened:   func(c *baos.Client) { go describe(c) },
		Log:      log.Printf,
	})
	gateway.CloseOnSignal(a)

	servers := 0

	for _, out := range outputs {
		switch out.Scheme {
		case "knxip":
			serveKNXIP(out, knxipOptions{
				attach:       a.Attach,
				lineAddr:     addr &^ 0xFF,
				tunnels:      []cemi.IndividualAddr{addr},
				objectServer: a.Hub.ServeKNXIP,
				version:      version,
				trace:        trace,
			})
			servers++

		case "baos":
			serveBAOS(out, a.Hub)

		case "json":
			serveJSON(out, a.Hub)

		case "monitor":
			check(out.Unused())

			go gateway.PrintTelegrams(a.Bus.Attach(0))
			go printIndications(a.Hub)

		default:
			log.Fatalf("%s: not an output of baos_auto", out)
		}
	}

	if servers == 0 {
		log.Fatal("baos_auto needs a knxip output, whose tunnels switch the module to data link layer mode")
	}

	log.Printf("Device 1: the module at %v; \"p\" switches its programming mode", addr)
	commands([]progDevice{autoDevice{a, addr}})
}

// autoDevice is the module of a baos_auto input, whose programming mode is a server item in
// ObjectServer mode and a property of a cEMI module in data link layer mode.
type autoDevice struct {
	a    *gateway.AutoModule
	addr cemi.IndividualAddr
}

var (
	errSwitching  = errors.New("the module is switching modes")
	errNoProgMode = errors.New("an EMI2 module in data link layer mode has no programming mode to switch")
)

func (d autoDevice) Address() cemi.IndividualAddr { return d.addr }

// current returns the device in the mode that the module is in.
func (d autoDevice) current() (progDevice, error) {
	if line, ok := d.a.Line().(*ft12.Link); ok {
		if line.Protocol() != ft12.ProtocolCEMI {
			return nil, errNoProgMode
		}

		return moduleDevice{line}, nil
	}

	if c := d.a.Hub.Client(); c != nil {
		return serverDevice{c, d.addr}, nil
	}

	return nil, errSwitching
}

func (d autoDevice) ProgMode() (bool, error) {
	dev, err := d.current()
	if err != nil {
		return false, err
	}

	return dev.ProgMode()
}

func (d autoDevice) SetProgMode(on bool) error {
	dev, err := d.current()
	if err != nil {
		return err
	}

	return dev.SetProgMode(on)
}

// serverDevice is a module in ObjectServer mode, whose programming mode is a server item.
type serverDevice struct {
	c    *baos.Client
	addr cemi.IndividualAddr
}

func (s serverDevice) Address() cemi.IndividualAddr { return s.addr }

func (s serverDevice) ProgMode() (bool, error) {
	items, err := s.c.ServerItems(baos.ItemProgrammingMode, 1)
	if err != nil || len(items) != 1 || len(items[0].Data) != 1 {
		return false, fmt.Errorf("reading the programming mode: %v", err)
	}

	return items[0].Data[0]&1 != 0, nil
}

func (s serverDevice) SetProgMode(on bool) error {
	value := byte(0)
	if on {
		value = 1
	}

	return s.c.SetServerItems(baos.Item{ID: baos.ItemProgrammingMode, Data: []byte{value}})
}
