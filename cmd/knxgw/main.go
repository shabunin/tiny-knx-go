//go:build linux && !tinygo

// Command knxgw connects KNX media, like ffmpeg connects inputs to outputs: each -i reaches a real
// line, each -o serves the line to clients or prints it.
//
// In link mode the inputs pass telegrams. They are bridged to each other and to a virtual line,
// which the outputs serve:
//
//	knxgw -i ft12:/dev/ttyAMA0 -o knxip:wlan0                  # kBerry as a KNXnet/IP interface
//	knxgw -i ft12:/dev/ttyUSB0?device -o knxip:eth0 -o monitor   # EMI2 module hosted as a device
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
// ObjectServer mode. ft12 restores the previous mode when knxgw ends. baos_auto switches it, like an
// IP BAOS that is an ObjectServer and an interface at once: the module is in ObjectServer mode until
// ETS opens a tunnel, in data link layer mode while the tunnel is open, and back in ObjectServer
// mode a few seconds after it closed:
//
//	knxgw -i baos_auto:/dev/ttyAMA0 -o knxip:wlan0 -o baos:wlan0 -o baos:/run/knxgw.sock
//
// A cEMI module (kBerry) answers frames to its own address from the serial side with its own device
// stack, also in data link layer mode. The tunnel has the module's address, so ETS reaches the
// module through it like any device: "p" on standard input switches its programming mode, and ETS
// can assign its address and download its BAOS application. An EMI2 module does not answer that
// way; with ft12:PORT?device, a device with its mask and serial number is hosted for it instead, so
// that ETS can assign its address through another interface.
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
	"github.com/shabunin/tiny-knx-go/link/ft12"
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
  baos:/PATH                        ObjectServer over a unix socket, the same frames
  monitor                           print the datapoint value indications

Auto mode input:
  baos_auto:PORT[?baud=19200&idle=5s]
                                    BAOS module in ObjectServer mode, switched to data link
                                    layer mode while a tunnel is open and for idle after

Auto mode outputs: knxip (at least one), baos and monitor as above. While ETS has a tunnel, the
ObjectServer is gone and its clients are turned away.

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
		if in.Scheme == "baos" || in.Scheme == "baos_auto" {
			baosInputs++
		}
	}

	switch {
	case baosInputs == 0:
		runLink(inputs, outputs, *trace)

	case baosInputs == 1 && len(inputs) == 1 && inputs[0].Scheme == "baos_auto":
		runAuto(inputs[0], outputs, *trace)

	case baosInputs == 1 && len(inputs) == 1:
		runBAOS(inputs[0], outputs)

	default:
		log.Fatal("BAOS mode takes one baos or baos_auto input and no other")
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
		devices []progDevice
		alias   = lineAddr // the interface address of the first server
		tunnels = defaultTunnels
	)

	for i, spec := range servers {
		addr, list := serveKNXIP(func(addr cemi.IndividualAddr) link.Link { return bus.Attach(addr) }, spec, lineAddr, defaultTunnels, trace)
		if i == 0 {
			alias, tunnels = addr, list
		}
	}

	for _, l := range lines {
		switch {
		case l.Module == nil:
			continue

		case l.Spec.Has("device"):
			if l.Module.Protocol() == ft12.ProtocolCEMI {
				log.Printf("%s: the device hides the module, which answers for itself through the tunnel of its address", l.Spec)
			}

			d, kind, err := gateway.StartModuleDevice(bus, len(devices)+1, l.Module, tunnels, alias)
			if err != nil {
				log.Fatal(err)
			}

			devices = append(devices, hostedDevice{d})
			log.Printf("Device %d: %v %s; \"p %d\" switches its programming mode", len(devices), d.Address(), kind, len(devices))

		case l.Module.Protocol() == ft12.ProtocolCEMI:
			// A cEMI module answers frames to its own address from the serial side with its own
			// stack, in data link layer mode too, so ETS reaches it through the tunnel of that
			// address: it can assign the module's address and download its application.
			devices = append(devices, moduleDevice{l.Module})
			log.Printf("Device %d: the module at %v; \"p %d\" switches its programming mode", len(devices), l.Module.Address(), len(devices))
		}
	}

	commands(devices)
}

// serveKNXIP starts a tunnelling server of a knxip output and returns its address and tunnels.
func serveKNXIP(attach func(cemi.IndividualAddr) link.Link, spec *gateway.Spec, lineAddr cemi.IndividualAddr, defaultTunnels []cemi.IndividualAddr, trace bool) (cemi.IndividualAddr, []cemi.IndividualAddr) {
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
		info.SerialNumber = gateway.HostSerial(manufacturer, ifi.HardwareAddr, port)
	}

	config := knxip.TunnelServerConfig{
		Server: knxnet.ServerConfig{Device: info, Tunnels: tunnels},
		Attach: attach,
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

// A progDevice is a device whose programming mode can be switched from standard input.
type progDevice interface {
	Address() cemi.IndividualAddr
	ProgMode() (bool, error)
	SetProgMode(on bool) error
}

// hostedDevice is a device on the virtual line.
type hostedDevice struct{ d *device.Device }

func (h hostedDevice) Address() cemi.IndividualAddr { return h.d.Address() }

func (h hostedDevice) ProgMode() (bool, error) { return h.d.ProgMode(), nil }

func (h hostedDevice) SetProgMode(on bool) error {
	h.d.SetProgMode(on)
	return nil
}

// moduleDevice is the device stack of a cEMI module, whose programming mode is a property of its
// device object.
type moduleDevice struct{ m *ft12.Link }

func (m moduleDevice) Address() cemi.IndividualAddr { return m.m.Address() }

func (m moduleDevice) ProgMode() (bool, error) {
	data, err := m.m.ReadProperty(cemi.ObjectTypeDevice, 1, cemi.PIDProgMode, 1, 1)
	if err != nil || len(data) != 1 {
		return false, fmt.Errorf("reading the programming mode: %v", err)
	}

	return data[0]&1 != 0, nil
}

func (m moduleDevice) SetProgMode(on bool) error {
	value := byte(0)
	if on {
		value = 1
	}

	return m.m.WriteProperty(cemi.ObjectTypeDevice, 1, cemi.PIDProgMode, 1, []byte{value})
}

// commands reads commands for the devices from standard input.
func commands(devices []progDevice) {
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

			on, err := d.ProgMode()
			if err != nil {
				log.Printf("Device %d: %v", n, err)
				continue
			}

			on = !on
			if fields[2] != "" {
				on = fields[2] == "on"
			}

			if err := d.SetProgMode(on); err != nil {
				log.Printf("Device %d: setting the programming mode: %v", n, err)
				continue
			}

			log.Printf("Device %d: programming mode %v", n, on)

		case "l":
			for i, d := range devices {
				on, err := d.ProgMode()
				if err != nil {
					fmt.Printf("  device %d: %v, %v\n", i+1, d.Address(), err)
					continue
				}

				fmt.Printf("  device %d: %v, programming mode %v\n", i+1, d.Address(), on)
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

	hub := gateway.NewBAOSHub()
	gone := hub.Use(c)
	describe(c)

	for _, out := range outputs {
		switch out.Scheme {
		case "baos":
			serveBAOS(out, hub)

		case "monitor":
			check(out.Unused())
			go printIndications(hub)

		default:
			log.Fatalf("%s: not an output of BAOS mode", out)
		}
	}

	<-gone
	log.Fatal("The connection to the ObjectServer closed")
}

// serveBAOS serves the ObjectServer of the hub at a baos output.
func serveBAOS(spec *gateway.Spec, hub *gateway.BAOSHub) {
	port, err := spec.Int("port", gateway.BAOSPort)
	check(err)
	check(spec.Unused())

	l, err := gateway.ListenBAOS(spec.Target, port)
	check(err)

	gateway.CloseOnSignal(l) // removes a unix socket
	log.Printf("Serving the ObjectServer at %v", l.Addr())

	go func() { log.Fatal(gateway.ServeBAOS(l, hub)) }()
}

// printIndications prints the datapoint values and server items that the ObjectServer indicates.
func printIndications(hub *gateway.BAOSHub) {
	indications, _ := hub.Subscribe()

	for ind := range indications {
		if ind.Restarted {
			log.Print("The module restarted")

			if c := hub.Client(); c != nil {
				describe(c)
			}
		}

		for _, v := range ind.Values {
			log.Printf("DP %d = % x", v.ID, v.Data)
		}

		for _, item := range ind.Items {
			log.Printf("Item %d = % x", item.ID, item.Data)
		}
	}
}

// describe prints the identity of the ObjectServer and the number of datapoints.
func describe(c *baos.Client) {
	items, err := c.ServerItems(baos.ItemHardwareType, baos.ItemProgrammingMode)
	if err != nil {
		log.Printf("Reading the server items: %v", err)
		return
	}

	byID := map[uint16][]byte{}
	for _, item := range items {
		byID[item.ID] = item.Data
	}

	count := uint16(1000)
	if c.Version() == baos.Version1 {
		count = 255
	}

	descriptions, err := c.Descriptions(1, count)
	if err != nil {
		log.Printf("Reading the datapoint descriptions: %v", err)
		return
	}

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
