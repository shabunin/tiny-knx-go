package vbus

import (
	"bytes"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/shabunin/tiny-knx-go/apdu"
	"github.com/shabunin/tiny-knx-go/cemi"
	"github.com/shabunin/tiny-knx-go/device"
	"github.com/shabunin/tiny-knx-go/dpt"
	"github.com/shabunin/tiny-knx-go/ets/product"
	"github.com/shabunin/tiny-knx-go/link/mock"
)

// params is a parameter segment as ETS downloads it.
type params []byte

func (p params) ParamBytes(offset, n int) []byte {
	if offset+n > len(p) {
		return nil
	}

	return p[offset : offset+n]
}

// set stores the named parameter of the product, big-endian.
func (p params) set(prod *product.Product, name string, value int) {
	for _, b := range prod.Blocks {
		for _, param := range b.Params {
			if param.Name == name {
				for i := param.Bits/8 - 1; i >= 0; i-- {
					p[param.Offset+i] = byte(value)
					value >>= 8
				}

				return
			}
		}
	}

	panic("no parameter " + name)
}

// defaults returns the default parameters of the product.
func defaults(prod *product.Product) params {
	p := make(params, prod.ParamSize())

	for _, b := range prod.Blocks {
		for _, param := range b.Params {
			p.set(prod, param.Name, param.Default)
		}
	}

	return p
}

func ga(n int) cemi.GroupAddr {
	return cemi.NewGroupAddr3(1, 0, uint8(n))
}

type testBus struct {
	t      *testing.T
	bus    *mock.Bus
	env    *Env
	sender *mock.Link // receives everything the devices send
	next   uint8
}

func newTestBus(t *testing.T) *testBus {
	bus := mock.NewBus()
	sender := bus.Attach(cemi.NewIndividualAddr3(1, 1, 200))
	t.Cleanup(func() { sender.Close() })

	return &testBus{t: t, bus: bus, env: &Env{Rooms: NewRooms(1)}, sender: sender}
}

// start runs the application kind with the parameters p. addrs[i] is the group address of object
// i, 0 for none. The flags are those of the product.
func (tb *testBus) start(kind string, p params, addrs ...int) App {
	tb.t.Helper()
	tb.next++

	prod := Products[kind]
	addr := cemi.NewIndividualAddr3(1, 1, tb.next)

	d, err := device.New(device.Identity{OrderNumber: prod.OrderNumber}, device.Config{Address: addr, Objects: prod.Objects()})
	if err != nil {
		tb.t.Fatalf("device.New: %v", err)
	}

	var bindings []device.Binding
	for _, b := range prod.Blocks {
		for _, o := range b.Objects {
			binding := device.Binding{Flags: o.Flags}
			if i := len(bindings); i < len(addrs) && addrs[i] != 0 {
				binding.Addresses = []cemi.GroupAddr{ga(addrs[i])}
			}

			bindings = append(bindings, binding)
		}
	}

	d.Bind(bindings)

	app, err := New(kind, d, p, tb.env, func(format string, args ...any) {
		tb.t.Logf("%s: %s", kind, fmt.Sprintf(format, args...))
	})
	if err != nil {
		tb.t.Fatalf("New: %v", err)
	}

	l := tb.bus.Attach(addr)
	done := make(chan struct{})

	go func() {
		d.Run(l)
		close(done)
	}()

	tb.t.Cleanup(func() {
		app.Stop()
		l.Close()
		<-done
	})

	app.Configure()

	return app
}

func (tb *testBus) write(n int, value device.Packer) {
	tb.t.Helper()

	err := tb.sender.Send(&cemi.LDataReq{LData: cemi.LData{
		Control1:    cemi.Control1Request(cemi.PrioLow),
		Control2:    cemi.Control2GroupAddr | cemi.Control2Hops(6),
		Destination: uint16(ga(n)),
		Data:        apdu.Encode(apdu.GroupValueWrite{Data: value.Pack()}),
	}})
	if err != nil {
		tb.t.Fatalf("Send: %v", err)
	}
}

// expect waits up to timeout for a write of value to group address n, skipping other frames.
func (tb *testBus) expect(n int, value device.Packer, timeout time.Duration) {
	tb.t.Helper()

	want := value.Pack()
	deadline := time.After(timeout)

	for {
		select {
		case ind := <-tb.sender.Inbound():
			app, ok := ind.Data.(*cemi.AppData)
			if !ok || ind.Destination != uint16(ga(n)) {
				continue
			}

			a, err := apdu.Decode(app)
			if err != nil {
				tb.t.Fatalf("Decode: %v", err)
			}

			if w, ok := a.(apdu.GroupValueWrite); ok && bytes.Equal(w.Data, want) {
				return
			}

		case <-deadline:
			tb.t.Fatalf("No write of %x to %v", want, ga(n))
		}
	}
}

func TestProducts(t *testing.T) {
	for kind, p := range Products {
		if err := p.Check(); err != nil {
			t.Errorf("%s: %v", kind, err)
		}

		var b bytes.Buffer
		if err := p.WriteXML(&b); err != nil {
			t.Errorf("%s: WriteXML: %v", kind, err)
		}
	}
}

func TestNewChecksObjects(t *testing.T) {
	d, err := device.New(device.Identity{}, device.Config{Objects: Switch.Objects()})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := New("dimmer", d, params{}, &Env{}, t.Logf); err == nil {
		t.Error("New accepted a switch as a dimmer")
	}

	if _, err := New("toaster", d, params{}, &Env{}, t.Logf); err == nil {
		t.Error("New accepted an unknown kind")
	}
}

func TestSwitch(t *testing.T) {
	tb := newTestBus(t)

	p := defaults(Switch)
	p.set(Switch, "Brestart", 1)
	p.set(Switch, "Bstaircase", 1)
	p.set(Switch, "Btime", 1)

	app := tb.start("switch", p, 1, 2, 3, 4)

	if got, want := app.Status(), "A off, B on"; got != want {
		t.Errorf("Status after restart = %q, want %q", got, want)
	}

	tb.write(1, dpt.DPT_1001(true))
	tb.expect(2, dpt.DPT_1001(true), time.Second)

	tb.write(1, dpt.DPT_1001(false))
	tb.expect(2, dpt.DPT_1001(false), time.Second)

	// The staircase switches B off after a second.
	tb.write(3, dpt.DPT_1001(true))
	tb.expect(4, dpt.DPT_1001(true), time.Second)
	tb.expect(4, dpt.DPT_1001(false), 2*time.Second)

	if got, want := app.Status(), "A off, B off"; got != want {
		t.Errorf("Status = %q, want %q", got, want)
	}
}

func TestDimmer(t *testing.T) {
	tb := newTestBus(t)

	p := defaults(Dimmer)
	p.set(Dimmer, "on", 80)
	p.set(Dimmer, "min", 10)
	p.set(Dimmer, "speed", 1)

	app := tb.start("dimmer", p, 1, 2, 3, 4, 5)

	tb.write(1, dpt.DPT_1001(true))
	tb.expect(4, dpt.DPT_1001(true), time.Second)
	tb.expect(5, dpt.DPT_5001(80), time.Second)

	// Values below the minimum give the minimum.
	tb.write(3, dpt.DPT_5001(3))
	tb.expect(5, dpt.DPT_5001(10), time.Second)

	// Dimming up by the full range takes a second; stopping it reports where it stopped.
	tb.write(2, dpt.DPT_3007{Increase: true, StepCode: 1})
	time.Sleep(300 * time.Millisecond)
	tb.write(2, dpt.DPT_3007{Increase: true})

	var brightness float64
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := fmt.Sscanf(app.Status(), "on, %f %%", &brightness); err != nil {
			t.Fatalf("Status %q: %v", app.Status(), err)
		}

		if brightness > 10 || time.Now().After(deadline) {
			break
		}

		time.Sleep(10 * time.Millisecond)
	}

	time.Sleep(100 * time.Millisecond)
	if _, err := fmt.Sscanf(app.Status(), "on, %f %%", &brightness); err != nil || brightness < 25 || brightness > 60 {
		t.Errorf("Status after dimming for 0.3 s = %q, want about 40 %%", app.Status())
	}

	// Dimming down stops at the minimum.
	tb.write(2, dpt.DPT_3007{StepCode: 1})
	tb.expect(5, dpt.DPT_5001(10), 2*time.Second)

	tb.write(1, dpt.DPT_1001(false))
	tb.expect(4, dpt.DPT_1001(false), time.Second)

	if got := app.Status(); got != "off" {
		t.Errorf("Status = %q, want off", got)
	}
}

func TestButtons(t *testing.T) {
	tb := newTestBus(t)

	p := defaults(Buttons)
	p.set(Buttons, "B2function", buttonOn)
	p.set(Buttons, "B3function", buttonDim)
	p.set(Buttons, "B4function", buttonDisabled)

	app := tb.start("buttons", p, 1, 0, 2, 0, 3, 4, 4, 0).(Presser)

	// Toggling alternates.
	app.Press(1, 0)
	tb.expect(1, dpt.DPT_1001(true), time.Second)
	app.Press(1, 0)
	tb.expect(1, dpt.DPT_1001(false), time.Second)

	// A status from the actuator updates the switch object, so the next toggle switches off.
	tb.write(1, dpt.DPT_1001(true))
	time.Sleep(50 * time.Millisecond)
	app.Press(1, 0)
	tb.expect(1, dpt.DPT_1001(false), time.Second)

	app.Press(2, 0)
	tb.expect(2, dpt.DPT_1001(true), time.Second)

	// A long press dims up a light that is off, then down, and stops on release.
	app.Press(3, 50*time.Millisecond)
	tb.expect(4, dpt.DPT_3007{Increase: true, StepCode: 1}, time.Second)
	tb.expect(4, dpt.DPT_3007{Increase: true}, time.Second)

	tb.write(3, dpt.DPT_1001(true))
	time.Sleep(50 * time.Millisecond)
	app.Press(3, 50*time.Millisecond)
	tb.expect(4, dpt.DPT_3007{StepCode: 1}, time.Second)
	tb.expect(4, dpt.DPT_3007{}, time.Second)

	app.Press(3, 0)
	tb.expect(3, dpt.DPT_1001(false), time.Second)

	if err := app.Press(5, 0); err == nil {
		t.Error("Press(5) succeeded")
	}

	want := "1 toggle (off), 2 on (on), 3 dim (off), 4 disabled (off)"
	if got := app.Status(); got != want {
		t.Errorf("Status = %q, want %q", got, want)
	}
}

func TestHeater(t *testing.T) {
	tb := newTestBus(t)

	p := defaults(Heater)
	p.set(Heater, "room", 2)

	app := tb.start("heater", p, 1, 2, 3)

	tb.write(1, dpt.DPT_5001(50))
	tb.expect(3, dpt.DPT_5001(50), time.Second)

	if got, want := app.Status(), "room 2: valve 50 %, 1000 W"; got != want {
		t.Errorf("Status = %q, want %q", got, want)
	}

	tb.write(2, dpt.DPT_1001(true))
	tb.expect(3, dpt.DPT_5001(100), time.Second)

	// 2000 W keep a room at 30 °C.
	for i := 0; i < 24*3600; i++ {
		tb.env.Rooms.Step(time.Second)
	}

	if got := tb.env.Rooms.Temperature(2); math.Abs(got-30) > 0.01 {
		t.Errorf("Room 2 = %.2f °C, want 30 °C", got)
	}

	if got := tb.env.Rooms.Temperature(1); math.Abs(got-Outside) > 0.01 {
		t.Errorf("Room 1 = %.2f °C, want %.1f °C", got, Outside)
	}
}

// thermostat starts a thermostat whose clock the test drives.
func (tb *testBus) thermostat(p params) *thermostat {
	tb.t.Helper()

	saved := measureEvery
	measureEvery = time.Hour
	tb.t.Cleanup(func() { measureEvery = saved })

	return tb.start("thermostat", p, 1, 2, 3, 4).(*thermostat)
}

func TestThermostatPI(t *testing.T) {
	tb := newTestBus(t)
	th := tb.thermostat(defaults(Thermostat))

	now := time.Now()
	th.step(now)
	tb.expect(1, dpt.DPT_9001(18), time.Second)
	tb.expect(3, dpt.DPT_5001(100), time.Second)

	// The room is warmer than the setpoint: the output closes, and the temperature is sent when it
	// changed by 0.2 K.
	tb.env.Rooms.SetTemperature(1, 21.3)
	now = now.Add(controlEvery)
	th.step(now)
	tb.expect(1, dpt.DPT_9001(21.3), time.Second)
	tb.expect(3, dpt.DPT_5001(0), time.Second)

	tb.write(2, dpt.DPT_9001(22))
	time.Sleep(50 * time.Millisecond)

	if got, want := th.Status(), "room 1: 21.3 °C, setpoint 22.0 °C, heating 0 %"; got != want {
		t.Errorf("Status = %q, want %q", got, want)
	}
}

func TestThermostatTwoPoint(t *testing.T) {
	tb := newTestBus(t)

	p := defaults(Thermostat)
	p.set(Thermostat, "control", 1)
	p.set(Thermostat, "room", 3)
	th := tb.thermostat(p)

	steps := []struct {
		temp float64
		on   bool
	}{
		{20.5, true},
		{21.2, true}, // within the hysteresis
		{21.3, false},
		{20.8, false},
		{20.7, true},
	}

	now := time.Now()
	for _, s := range steps {
		tb.env.Rooms.SetTemperature(3, s.temp)
		th.step(now)
		now = now.Add(controlEvery)

		if got := th.Status(); got != fmt.Sprintf("room 3: %.1f °C, setpoint 21.0 °C, heating %s", s.temp, onOff(s.on)) {
			t.Errorf("At %.1f °C: Status = %q", s.temp, got)
		}
	}
}

// TestControlLoop runs a thermostat and a heater in the same room for two simulated hours.
func TestControlLoop(t *testing.T) {
	tb := newTestBus(t)
	th := tb.thermostat(defaults(Thermostat))
	h := tb.start("heater", defaults(Heater)).(*heatingActuator)

	now := time.Now()
	for i := 0; i < 2*3600; i++ {
		th.step(now)
		h.set(th.output)
		tb.env.Rooms.Step(time.Second)
		now = now.Add(time.Second)
	}

	if got := tb.env.Rooms.Temperature(1); math.Abs(got-21) > 0.2 {
		t.Errorf("Room 1 = %.2f °C, want 21 °C", got)
	}
}
