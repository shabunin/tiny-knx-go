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

	"github.com/shabunin/tiny-knx-go/apdu"
	"github.com/shabunin/tiny-knx-go/cemi"
	"github.com/shabunin/tiny-knx-go/device"
	"github.com/shabunin/tiny-knx-go/ets"
	"github.com/shabunin/tiny-knx-go/internal/pretty"
	"github.com/shabunin/tiny-knx-go/knxnet"
	"github.com/shabunin/tiny-knx-go/link"
	"github.com/shabunin/tiny-knx-go/link/knxip"
	"github.com/shabunin/tiny-knx-go/link/mock"
)

// The manufacturer of the hosted devices: 0x00FA, as in the demo products of the thelsing stack.
const manufacturer = 0x00FA

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
	flag.Parse()

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

	var hosted []*device.Device

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

	server, err := knxip.ListenTunnelServer(ifi, *port, knxip.TunnelServerConfig{
		Server: knxnet.ServerConfig{Device: info, Tunnels: tunnelAddrs},
		Attach: func(addr cemi.IndividualAddr) link.Link { return bus.Attach(addr) },
		Log:    log.Printf,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer server.Close()

	control := server.Control()
	log.Printf("Serving %q at %v:%d, interface %v, tunnels %s", *name, net.IP(control.IP[:]), control.Port, info.Address, *tunnels)

	listDevices(hosted)
	commands(hosted)
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
func startDevice(bus *mock.Bus, number int, addr cemi.IndividualAddr) *device.Device {
	d, err := device.New(device.Identity{
		SerialNumber: apdu.SerialNumber{manufacturer >> 8, manufacturer & 0xff, 0, 0, 0, byte(number)},
		OrderNumber:  "VBUS-" + strconv.Itoa(number),
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

	go func() {
		if err := d.Run(bus.Attach(addr)); err != nil {
			log.Printf("Device %d: %v", number, err)
		}
	}()

	return d
}

// startETSDevice runs a knx-demo temperature sensor that ETS programs. Its configuration is stored
// in dir, by serial number; a stored address replaces addr.
func startETSDevice(bus *mock.Bus, number int, addr cemi.IndividualAddr, dir string) *device.Device {
	serial := apdu.SerialNumber{manufacturer >> 8, manufacturer & 0xff, 0xde, 0x30, 0, byte(number)}
	storage := fileStorage(filepath.Join(dir, fmt.Sprintf("%x.bin", serial[:])))

	// The address is kept apart from the ETS image, like the device object in the thelsing stack.
	if stored, err := os.ReadFile(string(storage) + ".address"); err == nil {
		if a, err := cemi.NewIndividualAddrString(strings.TrimSpace(string(stored))); err == nil {
			addr = a
		}
	}

	d, err := device.New(device.Identity{SerialNumber: serial, OrderNumber: "IA4313"}, device.Config{
		Address: addr,
		Objects: []device.ObjectConfig{
			{Name: "current", DPT: "9.001"},
			{Name: "max", DPT: "9.001"},
			{Name: "min", DPT: "9.001"},
			{Name: "reset", DPT: "1.017"},
		},
	})
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

	e.OnConfigured(func() { go report() })
	report()

	for _, o := range d.Objects() {
		o.OnChange(func(o *device.Object) { log.Printf("Device %d: %s = % x", number, o.Name(), o.Value()) })
	}

	d.OnAddressChange(func(addr cemi.IndividualAddr) {
		log.Printf("Device %d: address %v", number, addr)
		if err := storage.saveAddress(addr); err != nil {
			log.Printf("Device %d: %v", number, err)
		}
	})
	d.OnProgModeChange(func(on bool) { log.Printf("Device %d: programming mode %v", number, on) })
	d.OnRestart(func() { log.Printf("Device %d: restart", number) })

	go func() {
		if err := d.Run(bus.Attach(d.Address())); err != nil {
			log.Printf("Device %d: %v", number, err)
		}
	}()

	return d
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

var listMu sync.Mutex

func listDevices(hosted []*device.Device) {
	listMu.Lock()
	defer listMu.Unlock()

	for i, d := range hosted {
		mode := ""
		if d.ProgMode() {
			mode = ", programming mode"
		}

		fmt.Printf("  device %d: %v%s\n", i+1, d.Address(), mode)
	}
}

// commands reads commands from standard input until it closes.
func commands(hosted []*device.Device) {
	scanner := bufio.NewScanner(os.Stdin)

	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}

		switch fields[0] {
		case "l":
			listDevices(hosted)

		case "p":
			n := 1
			if len(fields) > 1 {
				n, _ = strconv.Atoi(fields[1])
			}

			if n < 1 || n > len(hosted) {
				fmt.Println("No such device")
				continue
			}

			d := hosted[n-1]
			on := !d.ProgMode()

			if len(fields) > 2 {
				on = fields[2] == "on"
			}

			d.SetProgMode(on)

		default:
			fmt.Println("Commands: l (list devices), p N [on|off] (programming mode of device N)")
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
