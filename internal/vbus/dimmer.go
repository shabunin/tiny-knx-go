package vbus

import (
	"fmt"
	"math"
	"time"

	"github.com/shabunin/tiny-knx-go/device"
	"github.com/shabunin/tiny-knx-go/dpt"
)

// Objects of the dimmer.
const (
	dimSwitch = iota
	dimDimming
	dimBrightness
	dimStatusSwitch
	dimStatusBrightness
)

// dimStep is how often the dimmer moves its brightness.
const dimStep = 50 * time.Millisecond

// dimmer moves its brightness towards a target at the configured speed and reports the brightness
// when it stops. Relative dimming starts a movement by a step (step code 1 is the full range) and
// step code 0 stops it.
type dimmer struct {
	base

	brightness float64 // %
	target     float64
	reported   float64
}

func newDimmer(c config) *dimmer {
	m := &dimmer{base: base{config: c}, reported: -1}

	m.object(dimSwitch).OnChange(func(o *device.Object) {
		if boolValue(o) {
			m.moveTo(float64(m.param("on")), true)
		} else {
			m.moveTo(0, true)
		}
	})

	m.object(dimBrightness).OnChange(func(o *device.Object) {
		m.moveTo(percentValue(o), true)
	})

	m.object(dimDimming).OnChange(func(o *device.Object) {
		var v dpt.DPT_3007
		if o.Get(&v) == nil {
			m.dim(v)
		}
	})

	go m.run()

	return m
}

// Configure starts switched off.
func (m *dimmer) Configure() {
	m.moveTo(0, true)
}

// moveTo sets a new target, at once or by dimming. Brightness below the minimum is the minimum,
// except 0.
func (m *dimmer) moveTo(target float64, now bool) {
	if target > 0 {
		target = min(max(target, float64(m.param("min"))), 100)
	}

	m.mu.Lock()
	m.target = target
	if now {
		m.brightness = target
	}
	m.mu.Unlock()

	if now {
		m.report()
	}
}

func (m *dimmer) dim(v dpt.DPT_3007) {
	m.mu.Lock()
	current := m.brightness
	m.mu.Unlock()

	if v.StepCode == 0 {
		m.moveTo(current, true)
		return
	}

	step := 100 / math.Pow(2, float64(v.StepCode-1))

	if v.Increase {
		m.moveTo(math.Max(current, float64(m.param("min")))+step, false)
	} else {
		// Dimming down stops at the minimum, it does not switch off.
		m.moveTo(math.Max(current-step, float64(m.param("min"))), false)
	}
}

// run moves the brightness towards the target.
func (m *dimmer) run() {
	ticker := time.NewTicker(dimStep)
	defer ticker.Stop()

	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
		}

		m.mu.Lock()
		moving := m.brightness != m.target
		if moving {
			step := 100 * dimStep.Seconds() / float64(m.param("speed"))
			if math.Abs(m.target-m.brightness) <= step {
				m.brightness = m.target
			} else if m.target > m.brightness {
				m.brightness += step
			} else {
				m.brightness -= step
			}
		}
		stopped := moving && m.brightness == m.target
		m.mu.Unlock()

		if stopped {
			m.report()
		}
	}
}

// report sends the status if the brightness changed since the last report.
func (m *dimmer) report() {
	m.mu.Lock()
	brightness := m.brightness
	changed := brightness != m.reported
	m.reported = brightness
	m.mu.Unlock()

	if !changed {
		return
	}

	m.logf("%s", m.status(brightness))
	m.object(dimStatusSwitch).Set(dpt.DPT_1001(brightness > 0))
	m.object(dimStatusBrightness).Set(dpt.DPT_5001(brightness))
}

func (m *dimmer) status(brightness float64) string {
	if brightness == 0 {
		return "off"
	}

	return fmt.Sprintf("on, %.0f %%", brightness)
}

func (m *dimmer) Status() string {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.status(m.brightness)
}
