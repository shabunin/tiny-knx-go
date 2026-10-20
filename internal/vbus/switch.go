package vbus

import (
	"fmt"
	"strings"
	"time"

	"github.com/shabunin/tiny-knx-go/device"
	"github.com/shabunin/tiny-knx-go/dpt"
)

// switchActuator has two relays, each with a switch and a status object and an optional
// staircase timer that switches it off again.
type switchActuator struct {
	base

	on     [2]bool
	timers [2]*time.Timer
}

var switchChannels = [2]string{"A", "B"}

func newSwitch(c config) *switchActuator {
	s := &switchActuator{base: base{config: c}}

	for i := range switchChannels {
		s.object(2 * i).OnChange(func(o *device.Object) { s.set(i, boolValue(o)) })
	}

	return s
}

// Configure switches the relays to their state after bus voltage recovery.
func (s *switchActuator) Configure() {
	for i, name := range switchChannels {
		s.set(i, s.param(name+"restart") == 1)
	}
}

func (s *switchActuator) set(i int, on bool) {
	name := switchChannels[i]

	s.mu.Lock()
	changed := s.on[i] != on
	s.on[i] = on

	if s.timers[i] != nil {
		s.timers[i].Stop()
		s.timers[i] = nil
	}

	staircase := ""
	if on && s.param(name+"staircase") == 1 {
		d := time.Duration(s.param(name+"time")) * time.Second
		s.timers[i] = time.AfterFunc(d, func() { s.set(i, false) })
		staircase = fmt.Sprintf(" for %v", d)
	}
	s.mu.Unlock()

	if changed || staircase != "" {
		s.logf("channel %s %s%s", name, onOff(on), staircase)
	}

	s.object(2*i + 1).Set(dpt.DPT_1001(on))
}

func (s *switchActuator) Status() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var parts []string
	for i, name := range switchChannels {
		parts = append(parts, name+" "+onOff(s.on[i]))
	}

	return strings.Join(parts, ", ")
}

func (s *switchActuator) Stop() {
	s.mu.Lock()
	for _, t := range s.timers {
		if t != nil {
			t.Stop()
		}
	}
	s.mu.Unlock()

	s.base.Stop()
}
