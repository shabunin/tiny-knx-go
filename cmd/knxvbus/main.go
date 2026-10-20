//go:build !tinygo

// Command knxvbus runs a virtual KNX line with devices on it and serves it as a KNXnet/IP
// interface, so that ETS and other tools find it on the network and connect to it by tunnelling.
//
//	knxvbus -iface eth0                              # one device at 1.1.1
//	knxvbus -iface eth0 -devices 1.1.1,15.15.255     # a programmed and a new device
//	knxvbus -iface eth0 -monitor                     # print the telegrams on the line
//	knxvbus -iface eth0 -ets 15.15.255               # a device that ETS programs (knx-demo-tp)
//
// ETS devices implement the "Temperatursensor TP" of thelsing's knx-demo-tp.knxprod (manufacturer
// 0x00FA, application 0xABCE version 0x79). Their downloaded configuration is kept in -data.
//
// Commands on standard input switch the programming mode of a device, like its button:
//
//	p 1      # toggle programming mode of device 1 (see "l")
//	p 1 on   # switch it on (or off)
//	l        # list the devices
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shabunin/tiny-knx-go/apdu"
	"github.com/shabunin/tiny-knx-go/cemi"
	"github.com/shabunin/tiny-knx-go/device"
	"github.com/shabunin/tiny-knx-go/ets"
	"github.com/shabunin/tiny-knx-go/internal/pretty"
	"github.com/shabunin/tiny-knx-go/internal/vbus"
	"github.com/shabunin/tiny-knx-go/knxnet"
	"github.com/shabunin/tiny-knx-go/link"
	"github.com/shabunin/tiny-knx-go/link/knxip"
	"github.com/shabunin/tiny-knx-go/link/mock"
)

// The manufacturer of the hosted devices: 0x00FA, as in the demo products of the thelsing stack.
const manufacturer = 0x00FA

// The firmware revision of the hosted devices, shown by ETS as the firmware version 0.1.
const firmwareRevision = 1

func main() {
	iface := flag.String("iface", "", "network interface to serve on (default: all, searches on the default)")
	port := flag.Int("port", knxnet.Port, "UDP port for connections")
	address := flag.String("address", "1.1.0", "individual address of the interface")
	tunnels := flag.String("tunnels", "1.1.250,1.1.251,1.1.252,1.1.253", "individual addresses of the tunnels")
	devices := flag.String("devices", "1.1.1", "individual addresses of the hosted devices")
	name := flag.String("name", "tiny-knx-go virtual bus", "name shown in ETS")
	monitor := flag.Bool("monitor", false, "print the telegrams on the line")
	etsDevices := flag.String("ets", "", "individual addresses of ETS-programmable knx-demo devices")
	data := flag.String("data", "knxvbus-data", "directory for the configuration of ETS devices")
	trace := flag.Bool("trace", false, "print the KNXnet/IP packets")
	sim := flag.String("sim", "", "simulated devices that ETS programs, e.g. switch,dimmer@1.1.20 (types: "+simTypes()+")")
	speed := flag.Float64("speed", 5, "how much faster than real time room temperatures change")
	xmlDir := flag.String("xml", "", "write the ETS product databases of the simulated devices to this directory and exit")
	flag.Parse()

	if *xmlDir != "" {
		if err := writeProducts(*xmlDir); err != nil {
			log.Fatal(err)
		}

		return
	}

	var ifi *net.Interface

	if *iface != "" {
		var err error
		if ifi, err = net.InterfaceByName(*iface); err != nil {
			log.Fatal(err)
		}
	}

	info := knxnet.DeviceInfo{
		Medium:         0x02, // TP1
		Address:        parseAddress(*address),
		SerialNumber:   [6]byte{manufacturer >> 8, manufacturer & 0xff, 0xff, 0xff, 0, 1},
		MulticastGroup: knxnet.RoutingGroup,
		Name:           *name,
	}

	if ifi != nil {
		copy(info.MAC[:], ifi.HardwareAddr)
	}

	var tunnelAddrs []cemi.IndividualAddr
	for _, s := range strings.Split(*tunnels, ",") {
		tunnelAddrs = append(tunnelAddrs, parseAddress(s))
	}

	bus := mock.NewBus()

	if *monitor {
		go printTelegrams(bus.Attach(0))
	}

	var hosted []*hostedDevice

	for _, s := range strings.Split(*devices, ",") {
		if s = strings.TrimSpace(s); s != "" {
			hosted = append(hosted, startDevice(bus, len(hosted)+1, parseAddress(s)))
		}
	}

	for _, s := range strings.Split(*etsDevices, ",") {
		if s = strings.TrimSpace(s); s != "" {
			hosted = append(hosted, startETSDevice(bus, len(hosted)+1, parseAddress(s), *data))
		}
	}

	var rooms *vbus.Rooms

	if *sim != "" {
		rooms = vbus.NewRooms(*speed)
		go rooms.Run(nil)

		env := &vbus.Env{Rooms: rooms}

		for _, s := range strings.Split(*sim, ",") {
			if s = strings.TrimSpace(s); s != "" {
				hosted = append(hosted, startSimulated(bus, len(hosted)+1, s, *data, env))
			}
		}
	}

	config := knxip.TunnelServerConfig{
		Server: knxnet.ServerConfig{Device: info, Tunnels: tunnelAddrs},
		Attach: func(addr cemi.IndividualAddr) link.Link { return bus.Attach(addr) },
		Log:    log.Printf,
	}

	if *trace {
		config.Trace = printPacket
	}

	server, err := knxip.ListenTunnelServer(ifi, *port, config)
	if err != nil {
		log.Fatal(err)
	}
	defer server.Close()

	control := server.Control()
	log.Printf("Serving %q at %v:%d, interface %v, tunnels %s", *name, net.IP(control.IP[:]), control.Port, info.Address, *tunnels)

	listDevices(hosted, rooms)
	commands(hosted, rooms)
}

func parseAddress(s string) cemi.IndividualAddr {
	addr, err := cemi.NewIndividualAddrString(strings.TrimSpace(s))
	if err != nil {
		log.Fatalf("Invalid individual address %q: %v", s, err)
	}

	return addr
}

// startDevice runs a device with one switching object on the bus. The number makes its serial
// number unique.
func startDevice(bus *mock.Bus, number int, addr cemi.IndividualAddr) *hostedDevice {
	d, err := device.New(device.Identity{
		SerialNumber:     apdu.SerialNumber{manufacturer >> 8, manufacturer & 0xff, 0, 0, 0, byte(number)},
		OrderNumber:      "VBUS-" + strconv.Itoa(number),
		FirmwareRevision: firmwareRevision,
	}, device.Config{
		Address: addr,
		Objects: []device.ObjectConfig{
			{Name: "switch", DPT: "1.001", Flags: device.FlagCommunicate | device.FlagWrite | device.FlagRead | device.FlagTransmit},
		},
	})
	if err != nil {
		log.Fatal(err)
	}

	d.OnAddressChange(func(addr cemi.IndividualAddr) { log.Printf("Device %d: address %v", number, addr) })
	d.OnProgModeChange(func(on bool) { log.Printf("Device %d: programming mode %v", number, on) })
	d.OnRestart(func() { log.Printf("Device %d: restart", number) })
	run(bus, number, d)

	return &hostedDevice{d: d, kind: "switch object"}
}

// startETSDevice runs a knx-demo temperature sensor that ETS programs.
func startETSDevice(bus *mock.Bus, number int, addr cemi.IndividualAddr, dir string) *hostedDevice {
	serial := apdu.SerialNumber{manufacturer >> 8, manufacturer & 0xff, 0xde, 0x30, 0, byte(number)}

	d, _ := startProgrammable(bus, number, addr, dir, device.Identity{SerialNumber: serial, OrderNumber: "IA4313"}, []device.ObjectConfig{
		{Name: "current", DPT: "9.001"},
		{Name: "max", DPT: "9.001"},
		{Name: "min", DPT: "9.001"},
		{Name: "reset", DPT: "1.017"},
	}, nil)

	for _, o := range d.Objects() {
		o.OnChange(func(o *device.Object) { log.Printf("Device %d: %s = % x", number, o.Name(), o.Value()) })
	}

	run(bus, number, d)

	return &hostedDevice{d: d, kind: "knx-demo"}
}

// startProgrammable runs a device that ETS programs. Its configuration is stored in dir, by serial
// number; a stored address replaces addr. configured, if not nil, is called after each download.
// The caller runs the device.
func startProgrammable(bus *mock.Bus, number int, addr cemi.IndividualAddr, dir string, identity device.Identity, objects []device.ObjectConfig, configured func()) (*device.Device, *ets.Device) {
	serial := identity.SerialNumber
	storage := fileStorage(filepath.Join(dir, fmt.Sprintf("%x.bin", serial[:])))

	// The address is kept apart from the ETS image, like the device object in the thelsing stack.
	if stored, err := os.ReadFile(string(storage) + ".address"); err == nil {
		if a, err := cemi.NewIndividualAddrString(strings.TrimSpace(string(stored))); err == nil {
			addr = a
		}
	}

	identity.FirmwareRevision = firmwareRevision

	d, err := device.New(identity, device.Config{Address: addr, Objects: objects})
	if err != nil {
		log.Fatal(err)
	}

	e, err := ets.Attach(d, storage)
	if err != nil {
		log.Fatalf("Device %d: %v", number, err)
	}

	report := func() {
		if !e.Configured() {
			log.Printf("Device %d: not configured", number)
			return
		}

		v := e.ProgramVersion()
		log.Printf("Device %d: configured, program % x, parameters % x", number, v[:], e.ParamBytes(0, 8))

		for _, o := range d.Objects() {
			log.Printf("Device %d:   object %d %s %v %v", number, o.Index()+1, o.Name(), o.Flags(), d.Config().Objects[o.Index()].Addresses)
		}
	}

	e.OnConfigured(func() {
		go func() {
			report()

			if configured != nil {
				configured()
			}
		}()
	})
	report()

	d.OnAddressChange(func(addr cemi.IndividualAddr) {
		log.Printf("Device %d: address %v", number, addr)
		if err := storage.saveAddress(addr); err != nil {
			log.Printf("Device %d: %v", number, err)
		}
	})
	d.OnProgModeChange(func(on bool) { log.Printf("Device %d: programming mode %v", number, on) })
	d.OnRestart(func() { log.Printf("Device %d: restart", number) })

	return d, e
}

// run runs a device on the bus at its address.
func run(bus *mock.Bus, number int, d *device.Device) {
	go func() {
		if err := d.Run(bus.Attach(d.Address())); err != nil {
			log.Printf("Device %d: %v", number, err)
		}
	}()
}

// fileStorage keeps an ETS image in a file.
type fileStorage string

func (f fileStorage) Load() ([]byte, error) {
	image, err := os.ReadFile(string(f))
	if os.IsNotExist(err) {
		return nil, nil
	}

	return image, err
}

func (f fileStorage) saveAddress(addr cemi.IndividualAddr) error {
	if err := os.MkdirAll(filepath.Dir(string(f)), 0o755); err != nil {
		return err
	}

	return os.WriteFile(string(f)+".address", []byte(addr.String()), 0o644)
}

func (f fileStorage) Save(image []byte) error {
	if err := os.MkdirAll(filepath.Dir(string(f)), 0o755); err != nil {
		return err
	}

	return os.WriteFile(string(f), image, 0o644)
}

// A hostedDevice is a device on the virtual line, with its application if it is simulated.
type hostedDevice struct {
	d    *device.Device
	kind string
	app  vbus.App
}

var listMu sync.Mutex

func listDevices(hosted []*hostedDevice, rooms *vbus.Rooms) {
	listMu.Lock()
	defer listMu.Unlock()

	for i, h := range hosted {
		mode := ""
		if h.d.ProgMode() {
			mode = ", programming mode"
		}

		status := ""
		if h.app != nil {
			status = ": " + h.app.Status()
		}

		fmt.Printf("  device %d: %v %s%s%s\n", i+1, h.d.Address(), h.kind, mode, status)
	}

	if rooms != nil {
		var temps []string
		for room := 1; room <= vbus.NumRooms; room++ {
			temps = append(temps, fmt.Sprintf("%d: %.1f °C", room, rooms.Temperature(room)))
		}

		fmt.Printf("  rooms %s\n", strings.Join(temps, ", "))
	}
}

// commands reads commands from standard input until it closes.
func commands(hosted []*hostedDevice, rooms *vbus.Rooms) {
	scanner := bufio.NewScanner(os.Stdin)

	lookup := func(arg string) *hostedDevice {
		n, _ := strconv.Atoi(arg)
		if n < 1 || n > len(hosted) {
			fmt.Println("No such device")
			return nil
		}

		return hosted[n-1]
	}

	for scanner.Scan() {
		fields := append(strings.Fields(scanner.Text()), "", "", "", "")
		if fields[0] == "" {
			continue
		}

		switch fields[0] {
		case "l":
			listDevices(hosted, rooms)

		case "p":
			if fields[1] == "" {
				fields[1] = "1"
			}

			h := lookup(fields[1])
			if h == nil {
				continue
			}

			on := !h.d.ProgMode()
			if fields[2] != "" {
				on = fields[2] == "on"
			}

			h.d.SetProgMode(on)

		case "b":
			h := lookup(fields[1])
			if h == nil {
				continue
			}

			buttons, ok := h.app.(vbus.Presser)
			if !ok {
				fmt.Println("Not a push button")
				continue
			}

			button, _ := strconv.Atoi(fields[2])

			var hold time.Duration
			if fields[3] == "long" {
				hold = time.Second
				if secs, err := strconv.ParseFloat(fields[4], 64); err == nil {
					hold = time.Duration(secs * float64(time.Second))
				}
			}

			if err := buttons.Press(button, hold); err != nil {
				fmt.Println(err)
			}

		case "t":
			room, _ := strconv.Atoi(fields[1])
			temp, err := strconv.ParseFloat(fields[2], 64)
			if room < 1 || room > vbus.NumRooms || err != nil || rooms == nil {
				fmt.Println("Usage: t ROOM TEMP")
				continue
			}

			rooms.SetTemperature(room, temp)

		default:
			fmt.Println("Commands: l (list devices), p N [on|off] (programming mode of device N),")
			fmt.Println("  b N K [long [SECS]] (press button K of push button N), t ROOM TEMP (set a room temperature)")
		}
	}

	// Without a terminal, e.g. in the background, keep serving.
	select {}
}

// printTelegrams prints the frames on the bus.
func printTelegrams(l link.Link) {
	for ind := range l.Inbound() {
		log.Print(pretty.Frame(&ind.LData))
	}
}

// printPacket prints a KNXnet/IP packet that the server received or sent.
func printPacket(received bool, peer knxnet.HPAI, packet []byte) {
	direction := "->"
	if received {
		direction = "<-"
	}

	endpoint := func(h knxnet.HPAI) string { return fmt.Sprintf("%v:%d", net.IP(h.IP[:]), h.Port) }

	var text string

	service, err := knxnet.Decode(packet)
	switch s := service.(type) {
	case nil:
		text = fmt.Sprintf("% x (%v)", packet, err)
	case *knxnet.ConnectRequest:
		text = fmt.Sprintf("ConnectRequest type %d, control %s, data %s", s.Type, endpoint(s.Control), endpoint(s.Data))
	case *knxnet.ConnectResponse:
		text = fmt.Sprintf("ConnectResponse channel %d, %v, data %s, address %v", s.Channel, s.Status, endpoint(s.Data), s.Address)
	case *knxnet.ConnectionStateRequest:
		text = fmt.Sprintf("ConnectionStateRequest channel %d, control %s", s.Channel, endpoint(s.Control))
	case *knxnet.ConnectionStateResponse:
		text = fmt.Sprintf("ConnectionStateResponse channel %d, %v", s.Channel, s.Status)
	case *knxnet.DisconnectRequest:
		text = fmt.Sprintf("DisconnectRequest channel %d, control %s", s.Channel, endpoint(s.Control))
	case *knxnet.DisconnectResponse:
		text = fmt.Sprintf("DisconnectResponse channel %d, %v", s.Channel, s.Status)
	case *knxnet.TunnellingRequest:
		text = fmt.Sprintf("TunnellingRequest channel %d seq %d: %s", s.Channel, s.Sequence, message(s.Message))
	case *knxnet.TunnellingAck:
		text = fmt.Sprintf("TunnellingAck channel %d seq %d, %v", s.Channel, s.Sequence, s.Status)
	case *knxnet.DeviceConfigRequest:
		text = fmt.Sprintf("DeviceConfigRequest channel %d seq %d: %s", s.Channel, s.Sequence, message(s.Message))
	case *knxnet.DeviceConfigAck:
		text = fmt.Sprintf("DeviceConfigAck channel %d seq %d, %v", s.Channel, s.Sequence, s.Status)
	default:
		text = fmt.Sprintf("%T", service)
	}

	log.Printf("%s %s %s", direction, endpoint(peer), text)
}

// message describes a cEMI message.
func message(m cemi.Message) string {
	switch m := m.(type) {
	case *cemi.LDataReq:
		return "L_Data.req " + pretty.Frame(&m.LData)
	case *cemi.LDataInd:
		return "L_Data.ind " + pretty.Frame(&m.LData)
	case *cemi.LDataCon:
		if m.Control1&cemi.Control1HasError != 0 {
			return "L_Data.con (negative) " + pretty.Frame(&m.LData)
		}

		return "L_Data.con " + pretty.Frame(&m.LData)
	}

	return fmt.Sprintf("%T %+v", m, m)
}
