package vbus

import (
	"fmt"
	"strings"
	"time"

	"github.com/shabunin/tiny-knx-go/dpt"
)

// A Presser is the application of a push button, which knxvbus presses.
type Presser interface {
	App

	// Press presses button 1 to 4. A long press holds it for hold.
	Press(button int, hold time.Duration) error
}

// buttons sends on its switch objects and, for dimming, on its dimming objects. Its switch objects
// follow the status of the actuators (W and U flags), so that toggling switches the other way.
type buttons struct {
	base

	increase [4]bool // direction of the next dimming
}

func newButtons(c config) *buttons {
	return &buttons{base: base{config: c}}
}

func (p *buttons) Configure() {}

func (p *buttons) Press(button int, hold time.Duration) error {
	if button < 1 || button > 4 {
		return fmt.Errorf("no button %d", button)
	}

	i := button - 1
	name := fmt.Sprintf("B%d", button)
	sw, dim := p.object(2*i), p.object(2*i+1)
	on := boolValue(sw)

	switch p.param(name + "function") {
	case buttonDisabled:
		p.logf("button %d is disabled", button)

	case buttonToggle:
		p.send(button, !on)

	case buttonOn:
		p.send(button, true)

	case buttonOff:
		p.send(button, false)

	case buttonDim:
		if hold == 0 {
			p.send(button, !on)
			break
		}

		// A light that is off is dimmed up, one that is on alternately down and up.
		p.mu.Lock()
		increase := !on || p.increase[i]
		p.increase[i] = !increase
		p.mu.Unlock()

		p.logf("button %d held: dimming %s", button, map[bool]string{true: "up", false: "down"}[increase])
		dim.Set(dpt.DPT_3007{Increase: increase, StepCode: 1})

		time.AfterFunc(hold, func() {
			p.logf("button %d released", button)
			dim.Set(dpt.DPT_3007{Increase: increase})
		})
	}

	return nil
}

func (p *buttons) send(button int, on bool) {
	p.logf("button %d: switch %s", button, onOff(on))
	p.object(2 * (button - 1)).Set(dpt.DPT_1001(on))
}

func (p *buttons) Status() string {
	names := []string{"disabled", "toggle", "on", "off", "dim"}

	var parts []string
	for i := 0; i < 4; i++ {
		function := p.param(fmt.Sprintf("B%dfunction", i+1))
		parts = append(parts, fmt.Sprintf("%d %s (%s)", i+1, names[function], onOff(boolValue(p.object(2*i)))))
	}

	return strings.Join(parts, ", ")
}
