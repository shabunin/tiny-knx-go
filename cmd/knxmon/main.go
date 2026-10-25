//go:build linux && !tinygo

// Command knxmon talks to a KNX bus: it prints every telegram, sends group reads and writes and
// reads the identity of devices. It connects through a BAOS module (e.g. kBerry on a Raspberry
// Pi), a TP-UART/NCN5120, a KNXnet/IP tunnel or KNXnet/IP routing.
//
//	knxmon -port /dev/serial0
//	knxmon -port /dev/serial0 -write 1/1/1=1     # switch on (DPT 1) and exit
//	knxmon -port /dev/serial0 -write 1/1/2=0c1a  # 2 bytes, e.g. DPT 9 = 21.0 °C
//	knxmon -port /dev/serial0 -read 1/1/1        # read, then monitor to see the response
//	knxmon -port /dev/serial0 -info 1.1.3        # connect to a device and read its identity
//	knxmon -port /dev/serial0 -address 1.1.250   # change the module's own address
//	knxmon -port /dev/ttyUSB0 -tpuart            # TP-UART or NCN5120 instead of a BAOS module
//	knxmon -tunnel 192.168.1.10                  # KNXnet/IP tunnelling
//	knxmon -router -iface eth0 -address 1.1.254  # KNXnet/IP routing
//	knxmon -search                               # find KNXnet/IP interfaces and routers
//	knxmon -port /dev/serial0 -scan 1.1          # list the devices of line 1.1
//	knxmon -port /dev/serial0 -props 1.1.3       # read all properties of a device
package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/shabunin/tiny-knx-go/cemi"
	"github.com/shabunin/tiny-knx-go/internal/pretty"
	"github.com/shabunin/tiny-knx-go/knxnet"
	"github.com/shabunin/tiny-knx-go/link"
	"github.com/shabunin/tiny-knx-go/link/ft12"
	"github.com/shabunin/tiny-knx-go/link/knxip"
	"github.com/shabunin/tiny-knx-go/link/tpuart"
	"github.com/shabunin/tiny-knx-go/mgmt"
	"github.com/shabunin/tiny-knx-go/secure"
	"github.com/shabunin/tiny-knx-go/serial"
	"github.com/shabunin/tiny-knx-go/transport"
)

// busLink is what all links offer.
type busLink interface {
	link.Link
	link.Addresser
	Dropped() uint32
}

func main() {
	port := flag.String("port", "/dev/serial0", "serial port of the BAOS module (cEMI or EMI2) or TP-UART")
	baud := flag.Int("baud", 19200, "baud rate")
	useTPUART := flag.Bool("tpuart", false, "the serial port has a TP-UART or NCN5120")
	chip := flag.String("chip", "ncn5120", "TP-UART chip: ncn5120 or tpuart2")
	tunnel := flag.String("tunnel", "", "connect to this KNXnet/IP server (host[:port])")
	router := flag.Bool("router", false, "use KNXnet/IP routing (224.0.23.12)")
	iface := flag.String("iface", "", "network interface for routing and search")
	search := flag.Bool("search", false, "find KNXnet/IP interfaces and routers")
	scan := flag.String("scan", "", "list the devices of a line, e.g. 1.1")
	parallel := flag.Int("parallel", 8, "devices probed at the same time by -scan")
	props := flag.String("props", "", "read all properties of the device at this individual address")
	write := flag.String("write", "", "send a group write: GA=HEX, e.g. 1/1/1=1")
	read := flag.String("read", "", "send a group read to GA, e.g. 1/1/1")
	info := flag.String("info", "", "read the identity of the device at this individual address")
	progMode := flag.Bool("progmode", false, "list the devices in programming mode, until interrupted")
	memory := flag.String("memory", "", "dump memory of a device: IA:ADDRESS:COUNT in hex, e.g. 1.1.3:4000:40")
	key := flag.String("key", "", "authorize with this key (hex, e.g. ffffffff) before -memory")
	address := flag.String("address", "", "set our individual address (BAOS module, TP-UART, routing)")
	user := flag.Int("user", 2, "user ID for a secure tunnel, with -password")
	password := flag.String("password", "", "open the tunnel in a KNX IP Secure session with this user password")
	authCode := flag.String("auth", "", "device authentication code of the interface, checked if set (with -password)")
	flag.Parse()

	var ifi *net.Interface

	if *iface != "" {
		var err error
		if ifi, err = net.InterfaceByName(*iface); err != nil {
			log.Fatal(err)
		}
	}

	if *search {
		printServers(ifi)
		return
	}

	var ia cemi.IndividualAddr

	if *address != "" {
		var err error
		if ia, err = cemi.NewIndividualAddrString(*address); err != nil {
			log.Fatalf("Bad individual address %q: %v", *address, err)
		}
	}

	var l busLink

	switch {
	case *tunnel != "":
		config := knxip.DefaultTunnelConfig
		if *password != "" {
			config.Tunnel.Secure = &knxnet.TunnelSecure{User: uint8(*user), Password: secure.UserPasswordKey(*password)}
			if *authCode != "" {
				config.Tunnel.Secure.DeviceAuth = secure.DeviceAuthKey(*authCode)
			}
		}

		t, err := knxip.DialTunnel(*tunnel, config)
		if err != nil {
			log.Fatalf("Connecting to %s: %v", *tunnel, err)
		}

		if ia != 0 {
			log.Fatal("-address: the tunnel's address is assigned by the server")
		}

		l = t

	case *router:
		if ia == 0 {
			ia = cemi.NewIndividualAddr3(15, 15, 254)
		}

		r, err := knxip.ListenRouter(ifi, knxip.RouterConfig{Address: ia})
		if err != nil {
			log.Fatalf("Joining the routing group: %v", err)
		}

		l = r

	case *useTPUART:
		config := tpuart.DefaultConfig
		config.Address = ia

		if *chip == "tpuart2" {
			config.Chip = tpuart.ChipTPUART2
		}

		t, err := tpuart.Open(openSerial(*port, *baud), config)
		if err != nil {
			log.Fatalf("Opening TP-UART on %s: %v", *port, err)
		}

		l = t

	default:
		l = openBAOS(*port, *baud, ia)
	}

	defer l.Close()

	// Closing the link switches an EMI2 module back to normal mode.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-signals
		l.Close()
		os.Exit(1)
	}()

	log.Printf("Connected, individual address %s", l.Address())

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

		if *read == "" {
			return
		}
	}

	if *read != "" {
		send(l, *read, cemi.GroupValueRead, nil)
	}

	if *info != "" {
		printInfo(l, *info)
		return
	}

	if *scan != "" {
		scanLine(l, *scan, *parallel)
		return
	}

	if *progMode {
		listProgMode(l)
		return
	}

	if *memory != "" {
		dumpMemory(l, *memory, *key)
		return
	}

	if *props != "" {
		printProperties(l, *props)
		return
	}

	for ind := range l.Inbound() {
		fmt.Println(time.Now().Format("15:04:05.000"), pretty.Frame(&ind.LData))
	}

	if dropped := l.Dropped(); dropped > 0 {
		log.Printf("%d frames were dropped", dropped)
	}

	os.Exit(1)
}

func openSerial(path string, baud int) *os.File {
	file, err := serial.Open(path, baud, serial.ParityEven)
	if err != nil {
		log.Fatal(err)
	}

	return file
}

// openBAOS opens a BAOS module and, if ia is set, changes its address.
func openBAOS(path string, baud int, ia cemi.IndividualAddr) *ft12.Link {
	l, err := ft12.Open(openSerial(path, baud), ft12.DefaultConfig)
	if err != nil {
		log.Fatalf("Opening BAOS link on %s: %v", path, err)
	}

	if l.Protocol() == ft12.ProtocolEMI2 {
		log.Printf("Module ready, EMI2")
	} else if descriptor, err := l.ReadProperty(cemi.ObjectTypeDevice, 1, cemi.PIDDeviceDescriptor, 1, 1); err == nil {
		log.Printf("Module ready, cEMI, device descriptor %x", descriptor)
	} else {
		log.Printf("Module ready, cEMI, device descriptor unavailable: %v", err)
	}

	if ia != 0 {
		if err := l.SetAddress(ia); err != nil {
			log.Fatalf("Setting module address: %v", err)
		}
	}

	if ia, err := l.ReadAddress(); err == nil {
		log.Printf("Module address %s", ia)
	} else {
		log.Printf("Module address unavailable: %v", err)
	}

	return l
}

// dumpMemory prints memory of a device as hex lines, read in chunks that fit a standard frame.
func dumpMemory(l link.Link, spec, key string) {
	parts := strings.Split(spec, ":")
	if len(parts) != 3 {
		log.Fatal("-memory expects IA:ADDRESS:COUNT")
	}

	ia, err := cemi.NewIndividualAddrString(parts[0])
	if err != nil {
		log.Fatalf("Bad individual address %q: %v", parts[0], err)
	}

	address, err1 := strconv.ParseUint(parts[1], 16, 16)
	count, err2 := strconv.ParseUint(parts[2], 16, 16)

	if err1 != nil || err2 != nil {
		log.Fatal("-memory expects hex ADDRESS and COUNT")
	}

	client := transport.NewClient(l, transport.DefaultConfig)
	defer client.Close()

	if err := client.Connect(ia); err != nil {
		log.Fatalf("Connecting to %s: %v", ia, err)
	}
	defer client.Disconnect()

	if key != "" {
		k, err := strconv.ParseUint(key, 16, 32)
		if err != nil {
			log.Fatalf("Bad key %q", key)
		}

		level, err := client.Authorize(uint32(k))
		if err != nil {
			log.Fatalf("Authorizing: %v", err)
		}

		log.Printf("Access level %d", level)
	}

	end := address + count

	for line := address; line < end; line += 16 {
		var data []byte

		for at := line; at < line+16 && at < end; at += 12 {
			n := min(12, line+16-at, end-at)

			chunk, err := client.ReadMemory(uint16(at), uint8(n))
			if err != nil {
				fmt.Printf("%04x  (%v)\n", at, err)
				return
			}

			data = append(data, chunk...)
		}

		fmt.Printf("%04x  % x\n", line, data)
	}
}

// listProgMode asks for the devices in programming mode every few seconds, with a broadcast
// IndividualAddress_Read, and prints each one that answers for the first time.
func listProgMode(l busLink) {
	req := &cemi.LDataReq{LData: cemi.LData{
		Control1: cemi.Control1Request(cemi.PrioSystem),
		Control2: cemi.Control2GroupAddr | cemi.Control2Hops(6),
		Data:     &cemi.AppData{Command: cemi.IndividualAddrRequest},
	}}

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	seen := make(map[cemi.IndividualAddr]bool)

	log.Print("Waiting for devices in programming mode")

	for {
		if err := l.Send(req); err != nil {
			log.Printf("Sending IndividualAddress_Read: %v", err)
		}

	wait:
		for {
			select {
			case ind, open := <-l.Inbound():
				if !open {
					return
				}

				app, ok := ind.Data.(*cemi.AppData)
				if ok && app.Command == cemi.IndividualAddrResponse && !seen[ind.Source] {
					seen[ind.Source] = true
					fmt.Printf("%s  in programming mode\n", ind.Source)
				}

			case <-ticker.C:
				break wait
			}
		}
	}
}

func send(l link.Link, addr string, command cemi.APCI, data []byte) {
	ga, err := cemi.NewGroupAddrString(addr)
	if err != nil {
		log.Fatalf("Bad group address %q: %v", addr, err)
	}

	req := &cemi.LDataReq{LData: cemi.LData{
		Control1:    cemi.Control1Request(cemi.PrioLow),
		Control2:    cemi.Control2GroupAddr | cemi.Control2Hops(6),
		Destination: uint16(ga),
		Data:        &cemi.AppData{Command: command, Data: data},
	}}

	if err := l.Send(req); err != nil {
		log.Fatalf("Sending to %s: %v", ga, err)
	}

	log.Printf("Sent %s to %s", pretty.APCIName(command), ga)
}

// printInfo connects to a device and prints what identifies it.
func printInfo(l link.Link, addr string) {
	ia, err := cemi.NewIndividualAddrString(addr)
	if err != nil {
		log.Fatalf("Bad individual address %q: %v", addr, err)
	}

	client := transport.NewClient(l, transport.DefaultConfig)
	defer client.Close()

	if err := client.Connect(ia); err != nil {
		log.Fatalf("Connecting to %s: %v", ia, err)
	}
	defer client.Disconnect()

	mask, err := client.DeviceDescriptor()
	if err != nil {
		log.Fatalf("Reading device descriptor of %s: %v", ia, err)
	}

	fmt.Printf("Device %s\n", ia)
	fmt.Printf("  mask version    %04x\n", mask)

	// Properties of the device object (object index 0).
	properties := []struct {
		name string
		id   uint8
		text bool
	}{
		{"serial number", 11, false},
		{"manufacturer", 12, false},
		{"order number", 15, true},
		{"hardware type", 78, false},
	}

	for _, p := range properties {
		if value, err := client.ReadProperty(0, p.id, 1, 1); err == nil && p.text {
			fmt.Printf("  %-15s %q\n", p.name, strings.TrimRight(string(value), "\x00 "))
		} else if err == nil {
			fmt.Printf("  %-15s % x\n", p.name, value)
		} else {
			fmt.Printf("  %-15s (%v)\n", p.name, err)
		}
	}
}

// printServers lists the KNXnet/IP servers that answer a search.
func printServers(ifi *net.Interface) {
	servers, err := knxip.Search(ifi, 3*time.Second)
	if err != nil {
		log.Fatalf("Search: %v", err)
	}

	for _, s := range servers {
		var families []string

		for _, f := range s.Info.Families {
			if name, ok := familyNames[f.ID]; ok {
				families = append(families, fmt.Sprintf("%s v%d", name, f.Version))
			}
		}

		var secured []string
		for _, f := range s.Info.Secured {
			if name, ok := familyNames[f.ID]; ok {
				secured = append(secured, name)
			}
		}

		if len(secured) > 0 {
			families = append(families, "secure "+strings.Join(secured, " and "))
		}

		fmt.Printf("%-21s %-9s %q  %s\n", s.Control, s.Info.Address, s.Info.Name, strings.Join(families, ", "))
	}

	log.Printf("%d servers found", len(servers))
}

var familyNames = map[uint8]string{
	knxnet.FamilyCore:             "core",
	knxnet.FamilyDeviceManagement: "device management",
	knxnet.FamilyTunnelling:       "tunnelling",
	knxnet.FamilyRouting:          "routing",
	knxnet.FamilySecurity:         "security",
}

// scanLine lists the devices of a line, e.g. "1.1".
func scanLine(l link.Link, line string, parallel int) {
	var area, lineNo uint8
	if _, err := fmt.Sscanf(line, "%d.%d", &area, &lineNo); err != nil || area > 15 || lineNo > 15 {
		log.Fatalf("Bad line %q: want area.line, e.g. 1.1", line)
	}

	config := mgmt.DefaultScanConfig
	config.Parallel = parallel
	config.OnFound = func(f mgmt.Found) {
		if f.Err != nil {
			fmt.Printf("%-9s (%v)\n", f.Address, f.Err)
		} else {
			fmt.Printf("%-9s mask %04x\n", f.Address, f.Mask)
		}
	}

	start := time.Now()

	found, err := mgmt.Scan(l, mgmt.LineAddresses(area, lineNo), config)
	if err != nil {
		log.Fatalf("Scan: %v", err)
	}

	log.Printf("%d devices on line %s, scanned in %v", len(found), line, time.Since(start).Round(time.Second))
}

// printProperties connects to a device and prints the properties of all its interface objects.
func printProperties(l link.Link, addr string) {
	ia, err := cemi.NewIndividualAddrString(addr)
	if err != nil {
		log.Fatalf("Bad individual address %q: %v", addr, err)
	}

	client := transport.NewClient(l, transport.DefaultConfig)
	defer client.Close()

	if err := client.Connect(ia); err != nil {
		log.Fatalf("Connecting to %s: %v", ia, err)
	}
	defer client.Disconnect()

	properties, err := mgmt.ReadProperties(client, mgmt.PropertyOptions{Values: true})

	object := -1

	for _, p := range properties {
		if int(p.Object) != object {
			object = int(p.Object)
			fmt.Printf("Object %d: %s\n", p.Object, mgmt.ObjectTypeName(p.ObjectType))
		}

		access := "r "
		if p.WriteEnable {
			access = "rw"
		}

		value := fmt.Sprintf("% x", p.Value)
		if text, ok := asText(p.Type, p.Value); ok {
			value = fmt.Sprintf("%q", text)
		}

		switch {
		case p.Err != nil:
			value = fmt.Sprintf("(%v)", p.Err)
		case p.Elements == 0:
			value = "(empty)"
		case p.Elements > 1:
			value = fmt.Sprintf("[%d] %s", p.Elements, value)
		}

		fmt.Printf("  %3d %-24s %s pdt %02x max %-4d %s\n",
			p.PropertyID, mgmt.PropertyName(p.ObjectType, p.PropertyID), access, p.Type, p.MaxElements, value)
	}

	if err != nil {
		log.Fatalf("Reading properties of %s: %v", ia, err)
	}
}

// asText returns a property value as text if its datatype holds characters and it is printable.
func asText(pdt uint8, value []byte) (string, bool) {
	switch pdt {
	case 0x01, 0x02, 0x0C, 0x0E, 0x1A, 0x2F: // char, unsigned char, char blocks, generic 10, UTF-8
	default:
		return "", false
	}

	text := strings.TrimRight(string(value), "\x00")
	if len(text) < 2 {
		return "", false
	}

	for _, c := range text {
		if c < ' ' || c > '~' {
			return "", false
		}
	}

	return text, true
}
