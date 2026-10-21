package vbus

import (
	"fmt"

	"github.com/shabunin/tiny-knx-go/device"
	"github.com/shabunin/tiny-knx-go/dpt"
)

// Objects of the heating actuator.
const (
	heatValue = iota
	heatSwitch
	heatStatus
)

// heatingActuator opens its valve as its control value says, or fully when switched on, and gives
// the configured power to its room in proportion.
type heatingActuator struct {
	base

	valve float64 // %
}

func newHeater(c config) *heatingActuator {
	h := &heatingActuator{base: base{config: c}}

	h.object(heatValue).OnChange(func(o *device.Object) { h.set(percentValue(o)) })
	h.object(heatSwitch).OnChange(func(o *device.Object) {
		if boolValue(o) {
			h.set(100)
		} else {
			h.set(0)
		}
	})

	return h
}

// Configure keeps the valve where it is and applies it to the room and power of the parameters,
// which a download may have changed.
func (h *heatingActuator) Configure() {
	h.mu.Lock()
	valve := h.valve
	h.mu.Unlock()

	h.set(valve)
}

func (h *heatingActuator) set(valve float64) {
	h.mu.Lock()
	changed := valve != h.valve
	h.valve = valve
	h.mu.Unlock()

	h.env.Rooms.Heat(h, h.param("room"), h.watts()*valve/100)

	if changed {
		h.logf("valve %.0f %% (%.0f W in room %d)", valve, h.watts()*valve/100, h.param("room"))
	}

	h.object(heatStatus).Set(dpt.DPT_5001(valve))
}

func (h *heatingActuator) watts() float64 {
	return 1000 * float64(h.param("power"))
}

func (h *heatingActuator) Status() string {
	h.mu.Lock()
	defer h.mu.Unlock()

	return fmt.Sprintf("room %d: valve %.0f %%, %.0f W", h.param("room"), h.valve, h.watts()*h.valve/100)
}
