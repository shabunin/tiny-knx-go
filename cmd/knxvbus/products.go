//go:build !tinygo

package main

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/shabunin/tiny-knx-go/apdu"
	"github.com/shabunin/tiny-knx-go/cemi"
	"github.com/shabunin/tiny-knx-go/device"
	"github.com/shabunin/tiny-knx-go/ets"
	"github.com/shabunin/tiny-knx-go/ets/product"
	"github.com/shabunin/tiny-knx-go/internal/vbus"
	"github.com/shabunin/tiny-knx-go/link/mock"
)

// writeProducts writes the product database of each simulated device type to dir.
func writeProducts(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	var names []string
	for name := range vbus.Products {
		names = append(names, name)
	}

	sort.Strings(names)

	for _, name := range names {
		p := vbus.Products[name]

		var b bytes.Buffer
		if err := p.WriteXML(&b); err != nil {
			return err
		}

		path := filepath.Join(dir, p.OrderNumber+".xml")
		if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
			return err
		}

		log.Printf("%s: %s (%s)", path, p.Name, name)
	}

	return nil
}

// simTypes lists the simulated device types for the help.
func simTypes() string {
	var names []string
	for name := range vbus.Products {
		names = append(names, name)
	}

	sort.Strings(names)

	return strings.Join(names, ", ")
}

// startSimulated runs a simulated device that ETS programs. spec is its type, optionally with the
// address before programming, e.g. "dimmer@1.1.20"; the default is 15.15.255. Group addresses for
// its objects may follow, used until ETS programs it: "switch=1/1/1 1/1/2", "-" skips an object.
func startSimulated(bus *mock.Bus, number int, spec, dir string, env *vbus.Env) *hostedDevice {
	spec, groups, _ := strings.Cut(spec, "=")
	kind, at, _ := strings.Cut(spec, "@")

	p, ok := vbus.Products[kind]
	if !ok {
		log.Fatalf("Unknown device type %q, known are %s", kind, simTypes())
	}

	addr := cemi.NewIndividualAddr3(15, 15, 255)
	if at != "" {
		addr = parseAddress(at)
	}

	serial := apdu.SerialNumber{manufacturer >> 8, manufacturer & 0xff, byte(p.Application >> 8), byte(p.Application), 0, byte(number)}
	logf := func(format string, args ...any) {
		log.Printf("Device %d (%s): %s", number, kind, fmt.Sprintf(format, args...))
	}

	var app vbus.App
	params := &checkedParams{p: p, logf: logf}

	// Until ETS programs it, the objects with group addresses get the flags of the product.
	objects := p.Objects()

	var flags []device.Flags
	for _, b := range p.Blocks {
		for _, o := range b.Objects {
			flags = append(flags, o.Flags)
		}
	}

	for i, g := range strings.Fields(groups) {
		if g == "-" {
			continue
		}

		ga, err := cemi.NewGroupAddrString(g)
		if err != nil || i >= len(objects) {
			log.Fatalf("Device %d (%s): bad group address %q for object %d", number, kind, g, i+1)
		}

		objects[i].Addresses = []cemi.GroupAddr{ga}
		objects[i].Flags = flags[i]
	}

	d, e := startProgrammable(bus, number, addr, dir, device.Identity{SerialNumber: serial, OrderNumber: p.OrderNumber}, objects, func() {
		params.check()
		app.Configure()
	})

	params.e = e
	params.check()

	app, err := vbus.New(kind, d, params, env, logf)
	if err != nil {
		log.Fatal(err)
	}

	app.Configure()
	run(bus, number, d)

	return &hostedDevice{d: d, kind: kind, app: app}
}

// checkedParams are the parameters of a device if ETS loaded the application of its product,
// otherwise none, so that the application uses the defaults.
type checkedParams struct {
	e    *ets.Device
	p    *product.Product
	logf func(string, ...any)
}

func (c *checkedParams) ParamBytes(offset, n int) []byte {
	if !c.e.Configured() || c.e.ProgramVersion() != c.p.ProgramVersion() {
		return nil
	}

	return c.e.ParamBytes(offset, n)
}

// check warns if ETS loaded the application of another product.
func (c *checkedParams) check() {
	if v, want := c.e.ProgramVersion(), c.p.ProgramVersion(); c.e.Configured() && v != want {
		c.logf("ETS loaded application % x, not % x of %s: using the default parameters", v[:], want[:], c.p.OrderNumber)
	}
}
