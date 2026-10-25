//go:build linux && !tinygo

package main

import (
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/shabunin/tiny-knx-go/baos"
	"github.com/shabunin/tiny-knx-go/dpt"
	"github.com/shabunin/tiny-knx-go/link/ft12"
)

// runBAOS talks to the ObjectServer of a BAOS module (BAOS mode, see package baos): it prints the
// server items and the datapoints configured by ETS with their values, sends the commands of -set
// and -get, and prints value indications until interrupted.
func runBAOS(port string, baud int, set, get string) {
	conn := ft12.NewConn(openSerial(port, baud), ft12.DefaultConnConfig)
	if err := conn.Reset(); err != nil {
		log.Fatalf("Resetting the module on %s: %v", port, err)
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-signals
		conn.Close()
		os.Exit(1)
	}()

	c, err := baos.NewClient(conn, baos.DefaultConfig)
	if err != nil {
		log.Fatalf("Opening the ObjectServer on %s: %v", port, err)
	}

	printServerItems(c)

	descriptions := printDatapoints(c)

	if set != "" || get != "" {
		baosCommands(c, set, get)
	}

	log.Print("Waiting for indications")

	for ind := range c.Indications() {
		for _, v := range ind.Values {
			log.Printf("DP %d = %s", v.ID, formatValue(v, descriptions[v.ID]))
		}

		for _, item := range ind.Items {
			log.Printf("Item %d = % x", item.ID, item.Data)
		}
	}
}

func printServerItems(c *baos.Client) {
	last := uint16(15)
	if c.Version() == baos.Version2 {
		last = 60
	}

	items := map[uint16][]byte{}

	// Ask in small ranges, so that the answers fit into the buffer. A range that starts after the
	// last item of the server is rejected.
	for start := uint16(1); start <= last; start += 16 {
		list, err := c.ServerItems(start, min(16, last-start+1))
		if err != nil && start == 1 {
			log.Fatalf("Reading server items: %v", err)
		}

		if err != nil {
			break
		}

		for _, item := range list {
			items[item.ID] = item.Data
		}
	}

	version := func(id uint16) string {
		if b := items[id]; len(b) == 1 {
			return fmt.Sprintf("%d.%d", b[0]>>4, b[0]&0x0F)
		}

		return "?"
	}

	if c.Version() == baos.Version1 {
		fmt.Print("ObjectServer protocol 1.x")
	} else {
		fmt.Printf("ObjectServer protocol %s", version(baos.ItemProtocolVersion))
	}

	fmt.Printf(", firmware %s, hardware %s\n", version(baos.ItemFirmwareVersion), version(baos.ItemHardwareVersion))
	fmt.Printf("  serial number   % x\n", items[baos.ItemSerialNumber])
	fmt.Printf("  hardware type   % x\n", items[baos.ItemHardwareType])
	fmt.Printf("  application     % x % x version %s\n", items[baos.ItemApplicationVendor], items[baos.ItemApplicationID], version(baos.ItemApplicationVersion))
	fmt.Printf("  bus connected   % x, programming mode % x, buffer % x\n", items[baos.ItemBusConnected], items[baos.ItemProgrammingMode], items[baos.ItemBufferSize])

	for id := uint16(baos.ItemIndicationSending); id <= last; id++ {
		if data, ok := items[id]; ok {
			fmt.Printf("  item %-11d % x\n", id, data)
		}
	}
}

// printDatapoints prints the configured datapoints with their values and returns their
// descriptions.
func printDatapoints(c *baos.Client) map[uint16]baos.Description {
	count := uint16(1000)
	if c.Version() == baos.Version1 {
		count = 255
	}

	list, err := c.Descriptions(1, count)
	if err != nil {
		log.Fatalf("Reading datapoint descriptions: %v", err)
	}

	descriptions := map[uint16]baos.Description{}

	var last uint16

	for _, d := range list {
		if d.DPT != baos.DPTDisabled || c.Version() == baos.Version2 {
			descriptions[d.ID] = d
			last = max(last, d.ID)
		}
	}

	values := map[uint16]baos.Value{}

	if last > 0 {
		list, err := c.Values(1, last, baos.FilterAll)
		if err != nil {
			log.Printf("Reading datapoint values: %v", err)
		}

		for _, v := range list {
			values[v.ID] = v
		}
	}

	fmt.Printf("%d datapoints\n", len(descriptions))

	for id := uint16(1); id <= last; id++ {
		d, ok := descriptions[id]
		if !ok {
			continue
		}

		value := "no value"
		if v, ok := values[id]; ok {
			value = formatValue(v, d)
		}

		fmt.Printf("  %4d  DPT %-3d %2d bits  %s  %s\n", id, d.DPT.Main(), d.ValueType.Bits(), d.Flags, value)
	}

	return descriptions
}

// formatValue returns the value in hex, decoded if its DPT is known.
func formatValue(v baos.Value, d baos.Description) string {
	s := fmt.Sprintf("% x", v.Data)
	if !v.State.Valid {
		return s + " (unknown)"
	}

	if main := d.DPT.Main(); main != 0 {
		for _, sub := range []string{".001", ".000", ".010", ".600"} {
			datapoint, ok := dpt.Produce(strconv.Itoa(main) + sub)
			if !ok {
				continue
			}

			// Group values of more than 6 bits follow the APCI byte.
			data := v.Data
			if d.ValueType >= baos.Bytes1 {
				data = append([]byte{0}, data...)
			}

			if datapoint.Unpack(data) == nil {
				s += "  " + fmt.Sprint(datapoint)
			}

			break
		}
	}

	if v.State.Transmission != baos.TransmissionIdle {
		s += fmt.Sprintf(" (transmission %d)", v.State.Transmission)
	}

	return s
}

// baosCommands sets and sends the values of -set (ID=HEX,...) and reads the datapoints of -get
// (ID,...) from the bus.
func baosCommands(c *baos.Client, set, get string) {
	for _, s := range strings.Split(set, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}

		idText, valueText, _ := strings.Cut(s, "=")
		id, err := strconv.ParseUint(idText, 10, 16)

		value, herr := hex.DecodeString(valueText)
		if err != nil || herr != nil || len(value) == 0 {
			log.Fatalf("Bad -set %q: want ID=HEX", s)
		}

		if err := c.SetValues(baos.SetValue{ID: uint16(id), Command: baos.CommandSetAndSend, Data: value}); err != nil {
			log.Printf("Setting DP %d: %v", id, err)
		} else {
			log.Printf("DP %d set to % x and sent", id, value)
		}
	}

	for _, s := range strings.Split(get, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}

		id, err := strconv.ParseUint(s, 10, 16)
		if err != nil {
			log.Fatalf("Bad -get %q: want a datapoint ID", s)
		}

		if err := c.SetValues(baos.SetValue{ID: uint16(id), Command: baos.CommandRead}); err != nil {
			log.Printf("Reading DP %d from the bus: %v", id, err)
		} else {
			log.Printf("DP %d: GroupValueRead sent", id)
		}
	}
}
