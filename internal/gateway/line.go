package gateway

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/shabunin/tiny-knx-go/cemi"
	"github.com/shabunin/tiny-knx-go/internal/pretty"
	"github.com/shabunin/tiny-knx-go/knxnet"
	"github.com/shabunin/tiny-knx-go/link"
	"github.com/shabunin/tiny-knx-go/link/ft12"
	"github.com/shabunin/tiny-knx-go/link/knxip"
	"github.com/shabunin/tiny-knx-go/link/mock"
	"github.com/shabunin/tiny-knx-go/link/tpuart"
	"github.com/shabunin/tiny-knx-go/secure"
	"github.com/shabunin/tiny-knx-go/serial"
)

// An Uplink connects the virtual line to a real one.
type Uplink interface {
	link.Link
	link.Addresser
}

// A Line is an opened medium that reaches a real line.
type Line struct {
	Uplink
	Spec *Spec

	// Module is the serial module of an ft12 line.
	Module *ft12.Link
}

// LineMedia describes the media that OpenLine takes.
const LineMedia = `  ft12:PORT[?baud=19200&device]     BAOS module (cEMI, kBerry) or EMI2 interface; ETS reaches a
                                    cEMI module itself through the tunnel; device hosts an EMI2
                                    module as a device, so that ETS can assign its address
  tpuart:PORT[?baud=19200&parity=even|none&chip=ncn5120|tpuart2&address=15.15.255]
                                    TP-UART or NCN5120 transceiver; an NCN512x
                                    may be strapped for 8 bits without parity
  tunnel:HOST[:PORT][?user=2&password=&auth=]
                                    tunnel to another KNXnet/IP interface, secure with password
  routing:IFACE[?address=0.0.0]     KNXnet/IP routing (multicast 224.0.23.12)`

// OpenLine opens a medium of LineMedia. It is closed on SIGINT and SIGTERM, which switches an EMI2
// interface back to normal mode and frees a tunnel at once.
func OpenLine(spec *Spec) (*Line, error) {
	l := &Line{Spec: spec}

	switch spec.Scheme {
	case "ft12":
		baud, err := spec.Int("baud", 19200)
		if err != nil {
			return nil, err
		}

		spec.Has("device") // asked for by the caller

		module, err := OpenModule(spec.Target, baud)
		if err != nil {
			return nil, err
		}

		log.Printf("Real line through %s: %v module at %v", spec.Target, module.Protocol(), module.Address())
		l.Uplink, l.Module = module, module

	case "tpuart":
		config := tpuart.DefaultConfig

		baud, err := spec.Int("baud", 19200)
		if err != nil {
			return nil, err
		}

		if config.Address, err = spec.Address("address", 0); err != nil {
			return nil, err
		}

		switch chip := spec.Get("chip", "ncn5120"); chip {
		case "ncn5120":
		case "tpuart2":
			config.Chip = tpuart.ChipTPUART2
		default:
			return nil, fmt.Errorf("%s: unknown chip %q", spec, chip)
		}

		parity, err := ParseParity(spec.Get("parity", "even"))
		if err != nil {
			return nil, fmt.Errorf("%s: %v", spec, err)
		}

		port, err := serial.Open(spec.Target, baud, parity)
		if err != nil {
			return nil, err
		}

		t, err := tpuart.Open(port, config)
		if err != nil {
			port.Close()
			return nil, fmt.Errorf("opening the TP-UART on %s: %v", spec.Target, err)
		}

		log.Printf("Real line through %s: TP-UART at %v", spec.Target, t.Address())
		l.Uplink = t

	case "tunnel":
		config := knxip.DefaultTunnelConfig

		user, err := spec.Int("user", 2)
		if err != nil {
			return nil, err
		}

		password, auth := spec.Get("password", ""), spec.Get("auth", "")

		if password != "" {
			config.Tunnel.Secure = &knxnet.TunnelSecure{User: uint8(user), Password: secure.UserPasswordKey(password)}
			if auth != "" {
				config.Tunnel.Secure.DeviceAuth = secure.DeviceAuthKey(auth)
			}
		}

		t, err := knxip.DialTunnel(spec.Target, config)
		if err != nil {
			return nil, fmt.Errorf("connecting to %s: %v", spec.Target, err)
		}

		log.Printf("Real line through a tunnel to %s, address %v", spec.Target, t.Address())
		l.Uplink = t

	case "routing":
		ifi, err := Interface(spec.Target)
		if err != nil {
			return nil, err
		}

		config := knxip.DefaultRouterConfig
		if config.Address, err = spec.Address("address", 0); err != nil {
			return nil, err
		}

		r, err := knxip.ListenRouter(ifi, config)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", spec, err)
		}

		log.Printf("Real line through KNXnet/IP routing on %s", or(spec.Target, "the default interface"))
		l.Uplink = r

	default:
		return nil, fmt.Errorf("%s: unknown medium %q", spec, spec.Scheme)
	}

	if err := spec.Unused(); err != nil {
		l.Close()
		return nil, err
	}

	CloseOnSignal(l)

	return l, nil
}

// ParseParity parses the parity of a serial port: even, none or odd.
func ParseParity(s string) (serial.Parity, error) {
	switch s {
	case "even":
		return serial.ParityEven, nil
	case "none":
		return serial.ParityNone, nil
	case "odd":
		return serial.ParityOdd, nil
	}

	return 0, fmt.Errorf("unknown parity %q: want even, none or odd", s)
}

// OpenModule opens a serial module in data link layer mode and reads its address. Closing it
// restores the previous mode of a cEMI module and switches an EMI2 module to normal mode.
func OpenModule(name string, baud int) (*ft12.Link, error) {
	port, err := serial.Open(name, baud, serial.ParityEven)
	if err != nil {
		return nil, err
	}

	module, err := ft12.Open(port, ft12.DefaultConfig)
	if err != nil {
		port.Close()
		return nil, fmt.Errorf("opening the module on %s: %v", name, err)
	}

	if _, err := module.ReadAddress(); err != nil {
		module.Close()
		return nil, fmt.Errorf("reading the address of the module on %s: %v", name, err)
	}

	return module, nil
}

// Interface returns the network interface of this name, or nil for an empty name.
func Interface(name string) (*net.Interface, error) {
	if name == "" {
		return nil, nil
	}

	return net.InterfaceByName(name)
}

func or(s, def string) string {
	if s == "" {
		return def
	}

	return s
}

var (
	signalOnce sync.Once
	closersMu  sync.Mutex
	closers    []io.Closer
)

// CloseOnSignal closes c when the program is interrupted (SIGINT, SIGTERM), before it exits.
func CloseOnSignal(c io.Closer) {
	closersMu.Lock()
	closers = append(closers, c)
	closersMu.Unlock()

	signalOnce.Do(func() {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

		go func() {
			<-signals

			closersMu.Lock()
			for i := len(closers) - 1; i >= 0; i-- {
				closers[i].Close()
			}

			os.Exit(1)
		}()
	})
}

// Bridge passes frames between the virtual line and a real one. A virtual line may have several
// uplinks, which are then bridged to each other too; they must not reach the same real line, as
// frames would loop.
func Bridge(bus *mock.Bus, realLine Uplink, name string) {
	up := bus.AttachUplink()

	go func() {
		for ind := range realLine.Inbound() {
			up.Send(&cemi.LDataReq{LData: ind.LData})
		}

		log.Fatalf("The connection to the real line through %s closed", name)
	}()

	go func() {
		for ind := range up.Inbound() {
			if err := realLine.Send(&cemi.LDataReq{LData: ind.LData}); err != nil {
				log.Printf("To %s: %v: %s", name, err, pretty.Frame(&ind.LData))
			}
		}
	}()
}
