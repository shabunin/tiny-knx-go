//go:build linux && !tinygo

// Command knxgw connects KNX media, like ffmpeg connects inputs to outputs: each -i reaches a real
// line, each -o serves the line to clients or prints it.
//
// In link mode the inputs pass telegrams. They are bridged to each other and to a virtual line,
// which the outputs serve:
//
//	knxgw -i ft12:/dev/ttyAMA0 -o knxip:wlan0                  # kBerry as a KNXnet/IP interface
//	knxgw -i 'ft12:/dev/ttyAMA0?device' -o knxip:wlan0 -o monitor
//	knxgw -i tpuart:/dev/ttyUSB0?address=1.1.250 -o knxip:eth0 -o routing:eth0
//	knxgw -i tunnel:192.168.1.112 -o knxip:wlan0?port=3672      # repeat an interface elsewhere
//	knxgw -i routing:eth0 -o knxip:eth0?tunnels=1.1.250,1.1.251 # a tunnelling server for a router
//
// In BAOS mode the input is the ObjectServer of a BAOS module, with the datapoints that ETS
// configured, and the outputs serve it like an IP BAOS (TCP port 12004) or print its indications:
//
//	knxgw -i baos:/dev/ttyAMA0 -o baos:wlan0 -o monitor
//	knxgw -i baos:tcp://192.168.2.29 -o monitor
//
// A module is in one mode at a time: ft12 switches it to data link layer mode, baos to
// ObjectServer mode. ft12 restores the previous mode when knxgw ends.
//
// With ft12:PORT?device, the module itself is hosted as device 1. In programming mode (type "p" on
// standard input) ETS can assign it a new individual address through another interface.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"strings"

	"github.com/shabunin/tiny-knx-go/baos"
	"github.com/shabunin/tiny-knx-go/cemi"
	"github.com/shabunin/tiny-knx-go/device"
	"github.com/shabunin/tiny-knx-go/internal/gateway"
	"github.com/shabunin/tiny-knx-go/knxnet"
	"github.com/shabunin/tiny-knx-go/link"
	"github.com/shabunin/tiny-knx-go/link/knxip"
	"github.com/shabunin/tiny-knx-go/link/mock"
)

// The manufacturer in the serial numbers of the KNXnet/IP interfaces, as in knxvbus.
const manufacturer = 0x00FA

type specs []*gateway.Spec

func (s *specs) String() string { return fmt.Sprint(*s) }

func (s *specs) Set(text string) error {
	spec, err := gateway.ParseSpec(text)
	if err == nil {
		*s = append(*s, spec)
	}

	return err
}

const usage = `Usage: knxgw -i INPUT [-i INPUT...] -o OUTPUT [-o OUTPUT...]

Link mode inputs:
%s

Link mode outputs:
  knxip:[IFACE][?port=3671&name=&address=&tunnels=&password=&auth=]
                                    KNXnet/IP tunnelling server, which ETS finds by search;
                                    tunnels defaults to the addresses of the inputs, address
                                    to the line of the first input; password enables KNX IP
                                    Secure for users 1 (management) to 1+tunnels
  routing:IFACE[?address=0.0.0]     KNXnet/IP routing, the same as an input
  monitor                           print the telegrams

BAOS mode input:
%s

BAOS mode outputs:
  baos:[IFACE][?port=12004]         ObjectServer over TCP, like an IP BAOS
  monitor                           print the datapoint value indications

Options:
`

func main() {
	var inputs, outputs specs

	flag.Var(&inputs, "i", "input medium (repeatable)")
	flag.Var(&outputs, "o", "output (repeatable)")
	trace := flag.Bool("trace", false, "print the KNXnet/IP packets of the tunnelling servers")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), usage, gateway.LineMedia, gateway.BAOSMedia)
		flag.PrintDefaults()
	}
	flag.Parse()

	if len(inputs) == 0 || len(outputs) == 0 || flag.NArg() > 0 {
		flag.Usage()
		os.Exit(2)
	}

	baosInputs := 0
	for _, in := range inputs {
		if in.Scheme == "baos" {
			baosInputs++
		}
	}

	switch {
	case baosInputs == 0:
		runLink(inputs, outputs, *trace)

	case baosInputs == 1 && len(inputs) == 1:
		runBAOS(inputs[0], outputs)

	default:
		log.Fatal("BAOS mode takes one baos input and no other")
	}
}

func runLink(inputs, outputs specs, trace bool) {
	bus := mock.NewBus()

	var lines []*gateway.Line

	open := func(spec *gateway.Spec) {
		l, err := gateway.OpenLine(spec)
		if err != nil {
			log.Fatal(err)
		}

		gateway.Bridge(bus, l, spec.String())
		lines = append(lines, l)
	}

	for _, in := range inputs {
		open(in)
	}

	// Routing outputs are bridged like inputs, but have no address for tunnels.
	var servers []*gateway.Spec

	for _, out := range outputs {
		switch out.Scheme {
		case "routing":
			open(out)

		case "knxip":
			servers = append(servers, out)

		case "monitor":
			if err := out.Unused(); err != nil {
				log.Fatal(err)
			}

			go gateway.PrintTelegrams(bus.Attach(0))

		default:
			log.Fatalf("%s: not an output of link mode", out)
		}
	}

	if len(inputs) > 1 {
		log.Printf("Bridging %d inputs: they must reach different lines, or frames loop between them", len(inputs))
	}

	var (
		defaultTunnels []cemi.IndividualAddr
		lineAddr       = cemi.NewIndividualAddr3(1, 1, 0)
	)

	for i, l := range lines {
		if addr := l.Address(); addr != 0 {
			defaultTunnels = append(defaultTunnels, addr)

			if len(defaultTunnels) == 1 {
				lineAddr = addr &^ 0xFF
			}
		}

		if l.Spec.Has("device") && l.Module == nil {
			log.Fatalf("%s: device works with ft12 only", inputs[i])
		}
	}

	if len(defaultTunnels) == 0 {
		defaultTunnels = []cemi.IndividualAddr{lineAddr | 250}
	}

	var (
		devices []*device.Device
		alias   = lineAddr // the interface address of the first server
		tunnels = defaultTunnels
	)

	for i, spec := range servers {
		addr, list := serveKNXIP(bus, spec, lineAddr, defaultTunnels, trace)
		if i == 0 {
			alias, tunnels = addr, list
		}
	}

	for _, l := range lines {
		if l.Module == nil || !l.Spec.Has("device") {
			continue
		}

		d, kind, err := gateway.StartModuleDevice(bus, len(devices)+1, l.Module, tunnels, alias)
		if err != nil {
			log.Fatal(err)
		}

		log.Printf("Device %d: %v %s; \"p %d\" switches its programming mode", len(devices)+1, d.Address(), kind, len(devices)+1)
		devices = append(devices, d)
	}

	commands(devices)
}

// serveKNXIP starts a tunnelling server of a knxip output and returns its address and tunnels.
func serveKNXIP(bus *mock.Bus, spec *gateway.Spec, lineAddr cemi.IndividualAddr, defaultTunnels []cemi.IndividualAddr, trace bool) (cemi.IndividualAddr, []cemi.IndividualAddr) {
	ifi, err := gateway.Interface(spec.Target)
	check(err)

	port, err := spec.Int("port", knxnet.Port)
	check(err)

	address, err := spec.Address("address", lineAddr)
	check(err)

	tunnels, err := spec.Addresses("tunnels")
	check(err)

	if len(tunnels) == 0 {
		tunnels = defaultTunnels
	}

	name := spec.Get("name", "tiny-knx-go gateway")
	password := spec.Get("password", "")
	authCode := spec.Get("auth", "")
	check(spec.Unused())

	info := knxnet.DeviceInfo{
		Medium:         0x02, // TP1
		Address:        address,
		SerialNumber:   gateway.InterfaceSerial(manufacturer, port),
		MulticastGroup: knxnet.RoutingGroup,
		Name:           name,
	}

	if ifi != nil {
		copy(info.MAC[:], ifi.HardwareAddr)
	}

	config := knxip.TunnelServerConfig{
		Server: knxnet.ServerConfig{Device: info, Tunnels: tunnels},
		Attach: func(addr cemi.IndividualAddr) link.Link { return bus.Attach(addr) },
		Log:    log.Printf,
	}

	if password != "" {
		config.Server.Secure = gateway.SecureConfig(password, authCode, len(tunnels))
		log.Printf("%s: KNX IP Secure, users 1 to %d with the password", spec.Target, 1+len(tunnels))
	}

	if trace {
		config.Trace = gateway.PrintPacket
	}

	server, err := knxip.ListenTunnelServer(ifi, port, config)
	check(err)

	gateway.CloseOnSignal(server)

	var list []string
	for _, t := range tunnels {
		list = append(list, t.String())
	}

	control := server.Control()
	log.Printf("Serving %q at %v:%d, interface %v, tunnels %s", name, net.IP(control.IP[:]), control.Port, address, strings.Join(list, ","))

	return address, tunnels
}

// commands reads commands for the hosted devices from standard input.
func commands(devices []*device.Device) {
	scanner := bufio.NewScanner(os.Stdin)

	for scanner.Scan() {
		fields := append(strings.Fields(scanner.Text()), "", "", "")

		switch fields[0] {
		case "":
		case "p":
			n := 1
			fmt.Sscan(fields[1], &n)

			if n < 1 || n > len(devices) {
				fmt.Println("No such device")
				continue
			}

			d := devices[n-1]

			on := !d.ProgMode()
			if fields[2] != "" {
				on = fields[2] == "on"
			}

			d.SetProgMode(on)

		case "l":
			for i, d := range devices {
				fmt.Printf("  device %d: %v, programming mode %v\n", i+1, d.Address(), d.ProgMode())
			}

		default:
			fmt.Println("Commands: l (list devices), p [N] [on|off] (programming mode of device N)")
		}
	}

	// Without a terminal, e.g. in the background, keep serving.
	select {}
}

func runBAOS(input *gateway.Spec, outputs specs) {
	c, err := gateway.OpenBAOS(input)
	check(err)

	hub := gateway.NewBAOSHub(c)
	describe(c)

	for _, out := range outputs {
		switch out.Scheme {
		case "baos":
			port, err := out.Int("port", gateway.BAOSPort)
			check(err)
			check(out.Unused())

			l, err := gateway.ListenBAOS(out.Target, port)
			check(err)

			log.Printf("Serving the ObjectServer at %v", l.Addr())

			go func() { log.Fatal(gateway.ServeBAOS(l, hub)) }()

		case "monitor":
			check(out.Unused())

			indications, _ := hub.Subscribe()

			go func() {
				for ind := range indications {
					for _, v := range ind.Values {
						log.Printf("DP %d = % x", v.ID, v.Data)
					}

					for _, item := range ind.Items {
						log.Printf("Item %d = % x", item.ID, item.Data)
					}
				}
			}()

		default:
			log.Fatalf("%s: not an output of BAOS mode", out)
		}
	}

	<-hub.Done()
	log.Fatal("The connection to the ObjectServer closed")
}

// describe prints the identity of the ObjectServer and the number of datapoints.
func describe(c *baos.Client) {
	items, err := c.ServerItems(baos.ItemHardwareType, baos.ItemProgrammingMode)
	check(err)

	byID := map[uint16][]byte{}
	for _, item := range items {
		byID[item.ID] = item.Data
	}

	count := uint16(1000)
	if c.Version() == baos.Version1 {
		count = 255
	}

	descriptions, err := c.Descriptions(1, count)
	check(err)

	configured := 0
	for _, d := range descriptions {
		if d.DPT != baos.DPTDisabled || c.Version() == baos.Version2 {
			configured++
		}
	}

	log.Printf("ObjectServer protocol %d, serial number % x, application % x, %d datapoints",
		c.Version(), byID[baos.ItemSerialNumber], byID[baos.ItemApplicationID], configured)
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
