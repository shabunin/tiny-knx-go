package vbus

import (
	"fmt"
	"math"
	"sync"

	"github.com/shabunin/tiny-knx-go/device"
	"github.com/shabunin/tiny-knx-go/dpt"
	"github.com/shabunin/tiny-knx-go/ets/product"
)

// An App is the firmware of a simulated device.
type App interface {
	// Configure reads the parameters. knxvbus calls it at start and after each download.
	Configure()

	// Status describes the state of the device, e.g. "A on, B off".
	Status() string

	// Stop ends the timers of the application.
	Stop()
}

// Env is what the applications share.
type Env struct {
	Rooms *Rooms
}

// New starts the application of the product kind (a key of Products) on d. params are the
// parameters downloaded by ETS (an *ets.Device), logf prints what the device does.
func New(kind string, d *device.Device, params product.Params, env *Env, logf func(string, ...any)) (App, error) {
	p, ok := Products[kind]
	if !ok {
		return nil, fmt.Errorf("vbus: unknown device type %q", kind)
	}

	if len(d.Objects()) != len(p.Objects()) {
		return nil, fmt.Errorf("vbus: device has %d objects, %s needs %d", len(d.Objects()), kind, len(p.Objects()))
	}

	b := config{d: d, p: p, params: params, env: env, logf: logf, stop: make(chan struct{})}

	switch kind {
	case "switch":
		return newSwitch(b), nil
	case "dimmer":
		return newDimmer(b), nil
	case "buttons":
		return newButtons(b), nil
	case "thermostat":
		return newThermostat(b), nil
	default:
		return newHeater(b), nil
	}
}

// config is what New gives an application.
type config struct {
	d      *device.Device
	p      *product.Product
	params product.Params
	env    *Env
	logf   func(string, ...any)
	stop   chan struct{}
}

// base holds what every application has.
type base struct {
	config

	mu       sync.Mutex
	stopOnce sync.Once
}

func (b *base) param(name string) int {
	return b.p.Value(b.params, name)
}

func (b *base) object(i int) *device.Object {
	return b.d.Objects()[i]
}

func (b *base) Stop() {
	b.stopOnce.Do(func() { close(b.stop) })
}

func boolValue(o *device.Object) bool {
	var v dpt.DPT_1001
	o.Get(&v)

	return bool(v)
}

// percentValue returns whole percent: DPT 5.001 has steps of 0.4 % and packing truncates, so that
// a fraction would lose a step each time it is sent on.
func percentValue(o *device.Object) float64 {
	var v dpt.DPT_5001
	o.Get(&v)

	return math.Round(float64(v))
}

func temperatureValue(o *device.Object) float64 {
	var v dpt.DPT_9001
	o.Get(&v)

	return float64(v)
}

func onOff(on bool) string {
	if on {
		return "on"
	}

	return "off"
}
