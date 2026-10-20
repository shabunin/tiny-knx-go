package vbus

import (
	"fmt"
	"math"
	"time"

	"github.com/shabunin/tiny-knx-go/device"
	"github.com/shabunin/tiny-knx-go/dpt"
)

// Objects of the thermostat.
const (
	thermoActual = iota
	thermoSetpoint
	thermoOutput
	thermoOnOff
)

// The PI controller: 50 % per kelvin of error, and the integral adds as much again every two
// minutes. The thermostat measures every measureEvery and controls every controlEvery.
const (
	gain         = 50.0
	integralTime = 120.0 // s
	controlEvery = 5 * time.Second
)

// measureEvery is a variable for the tests, which drive the thermostat themselves.
var measureEvery = time.Second

// thermostat measures the temperature of its room and drives the heating towards the setpoint,
// with a continuous or a two-point control value.
type thermostat struct {
	base

	setpoint float64
	actual   float64
	sent     float64 // last sent temperature
	sentAt   time.Time
	integral float64
	output   float64 // %, continuous
	heating  bool    // two-point
	lastRun  time.Time
	started  bool
}

func newThermostat(c config) *thermostat {
	th := &thermostat{base: base{config: c}}

	th.object(thermoSetpoint).OnChange(func(o *device.Object) {
		th.mu.Lock()
		th.setpoint = temperatureValue(o)
		th.mu.Unlock()

		th.logf("setpoint %.1f °C", temperatureValue(o))
	})

	go th.run(measureEvery)

	return th
}

// Configure starts with the setpoint of the parameters.
func (th *thermostat) Configure() {
	setpoint := float64(th.param("setpoint"))

	th.mu.Lock()
	th.setpoint = setpoint
	th.integral = 0
	th.started = false
	th.mu.Unlock()

	th.object(thermoSetpoint).Store(dpt.DPT_9001(setpoint))
}

func (th *thermostat) run(every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	for {
		select {
		case <-th.stop:
			return
		case now := <-ticker.C:
			th.step(now)
		}
	}
}

// step measures and, every controlEvery, controls.
func (th *thermostat) step(now time.Time) {
	temp := th.env.Rooms.Temperature(th.param("room"))

	th.mu.Lock()
	th.actual = temp
	first := !th.started
	th.started = true

	// Send the temperature at start, on change and cyclically.
	change := float64(th.param("change")) / 10
	cycle := time.Duration(th.param("cycle")) * time.Minute
	send := first ||
		change > 0 && math.Abs(temp-th.sent) >= change-1e-9 ||
		cycle > 0 && now.Sub(th.sentAt) >= cycle

	if send {
		th.sent, th.sentAt = temp, now
	}

	control := first || now.Sub(th.lastRun) >= controlEvery
	dt := now.Sub(th.lastRun).Seconds()
	if control {
		th.lastRun = now
	}
	th.mu.Unlock()

	if send {
		th.object(thermoActual).Set(dpt.DPT_9001(temp))
	} else {
		th.object(thermoActual).Store(dpt.DPT_9001(temp))
	}

	if control {
		if first {
			dt = 0
		}

		th.control(dt)
	}
}

func (th *thermostat) control(dt float64) {
	th.mu.Lock()
	diff := th.setpoint - th.actual

	if th.param("control") == 1 {
		hysteresis := float64(th.param("hysteresis")) / 10
		heating := th.heating

		if diff > hysteresis/2 {
			heating = true
		} else if diff < -hysteresis/2 {
			heating = false
		}

		changed := heating != th.heating || dt == 0
		th.heating = heating
		actual, setpoint := th.actual, th.setpoint
		th.mu.Unlock()

		if changed {
			th.logf("%.1f °C, setpoint %.1f °C: heating %s", actual, setpoint, onOff(heating))
			th.object(thermoOnOff).Set(dpt.DPT_1001(heating))
		}

		return
	}

	// The integral is limited to the output range, so that it does not wind up.
	th.integral = min(max(th.integral+gain*diff*dt/integralTime, 0), 100)
	output := math.Round(min(max(gain*diff+th.integral, 0), 100))

	changed := output != th.output || dt == 0
	th.output = output
	actual, setpoint := th.actual, th.setpoint
	th.mu.Unlock()

	if changed {
		th.logf("%.1f °C, setpoint %.1f °C: heating %.0f %%", actual, setpoint, output)
		th.object(thermoOutput).Set(dpt.DPT_5001(output))
	}
}

func (th *thermostat) Status() string {
	th.mu.Lock()
	defer th.mu.Unlock()

	heating := fmt.Sprintf("heating %.0f %%", th.output)
	if th.param("control") == 1 {
		heating = "heating " + onOff(th.heating)
	}

	return fmt.Sprintf("room %d: %.1f °C, setpoint %.1f °C, %s", th.param("room"), th.actual, th.setpoint, heating)
}
