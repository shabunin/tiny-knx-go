// Package vbus simulates a small installation for knxvbus: a switch actuator, a dimmer, push
// buttons, a thermostat and a heating actuator, all programmable by ETS, and the rooms whose
// temperature the heating changes and the thermostats measure.
package vbus

import (
	"fmt"

	"github.com/shabunin/tiny-knx-go/device"
	"github.com/shabunin/tiny-knx-go/ets/product"
)

const (
	c = device.FlagCommunicate
	r = device.FlagRead
	w = device.FlagWrite
	t = device.FlagTransmit
	u = device.FlagUpdate
)

var (
	enabled = []product.Value{{Value: 0, Text: "Disabled"}, {Value: 1, Text: "Enabled"}}
	rooms   = []product.Value{{Value: 1, Text: "Room 1"}, {Value: 2, Text: "Room 2"}, {Value: 3, Text: "Room 3"}, {Value: 4, Text: "Room 4"}}
)

// Switch is a 2-fold switch actuator with a staircase function.
var Switch = &product.Product{
	Name:        "Switch actuator 2-fold",
	OrderNumber: "TKGSA2",
	Application: 0x7401,
	Version:     0x10,
	Blocks:      []product.Block{switchChannel(0, "A"), switchChannel(1, "B")},
}

func switchChannel(i int, name string) product.Block {
	at := i * 4

	return product.Block{
		Name: "Channel " + name,
		Params: []product.Param{
			{Name: name + "restart", Text: "State after bus voltage recovery", Offset: at, Bits: 8,
				Values: []product.Value{{Value: 0, Text: "Off"}, {Value: 1, Text: "On"}}},
			{Name: name + "staircase", Text: "Staircase function", Offset: at + 1, Bits: 8, Values: enabled},
			{Name: name + "time", Text: "Staircase time (s)", Offset: at + 2, Bits: 16, Default: 60, Min: 1, Max: 3600},
		},
		Objects: []product.Object{
			{Name: name + ": Switch", Function: "On/Off", DPT: "1.001", Flags: c | w},
			{Name: name + ": Status", Function: "On/Off", DPT: "1.001", Flags: c | r | t},
		},
	}
}

// Dimmer is a universal dimmer with one channel.
var Dimmer = &product.Product{
	Name:        "Dimming actuator 1-fold",
	OrderNumber: "TKGDA1",
	Application: 0x7402,
	Version:     0x10,
	Blocks: []product.Block{{
		Name: "Dimmer",
		Params: []product.Param{
			{Name: "on", Text: "Brightness when switched on (%)", Offset: 0, Bits: 8, Default: 100, Min: 1, Max: 100},
			{Name: "min", Text: "Minimum brightness (%)", Offset: 1, Bits: 8, Default: 5, Min: 1, Max: 50},
			{Name: "speed", Text: "Dimming time 0-100 % (s)", Offset: 2, Bits: 8, Default: 4, Min: 1, Max: 30},
		},
		Objects: []product.Object{
			{Name: "Switch", Function: "On/Off", DPT: "1.001", Flags: c | w},
			{Name: "Dimming", Function: "Relative dimming", DPT: "3.007", Flags: c | w},
			{Name: "Brightness", Function: "Absolute value", DPT: "5.001", Flags: c | w},
			{Name: "Status switch", Function: "On/Off", DPT: "1.001", Flags: c | r | t},
			{Name: "Status brightness", Function: "Value", DPT: "5.001", Flags: c | r | t},
		},
	}},
}

// Buttons is a push button with four buttons, each switching, or switching and dimming.
var Buttons = &product.Product{
	Name:        "Push button 4-fold",
	OrderNumber: "TKGPB4",
	Application: 0x7403,
	Version:     0x10,
	Blocks:      []product.Block{button(0), button(1), button(2), button(3)},
}

// Functions of a push button.
const (
	buttonDisabled = iota
	buttonToggle
	buttonOn
	buttonOff
	buttonDim
)

func button(i int) product.Block {
	name := fmt.Sprintf("Button %d", i+1)
	prefix := fmt.Sprintf("B%d", i+1)

	return product.Block{
		Name: name,
		Params: []product.Param{
			{Name: prefix + "function", Text: "Function", Offset: i, Bits: 8, Default: buttonToggle, Values: []product.Value{
				{Value: buttonDisabled, Text: "Disabled"},
				{Value: buttonToggle, Text: "Switch: toggle"},
				{Value: buttonOn, Text: "Switch: on"},
				{Value: buttonOff, Text: "Switch: off"},
				{Value: buttonDim, Text: "Dimming: short toggle, long dim"},
			}},
		},
		Objects: []product.Object{
			// Status telegrams update the switch object, so that toggling follows the actuator.
			{Name: prefix + ": Switch", Function: "On/Off", DPT: "1.001", Flags: c | w | t | u},
			{Name: prefix + ": Dimming", Function: "Relative dimming", DPT: "3.007", Flags: c | t},
		},
	}
}

// Thermostat is a room temperature controller with a continuous (PI) or two-point output.
var Thermostat = &product.Product{
	Name:        "Room thermostat",
	OrderNumber: "TKGRT1",
	Application: 0x7404,
	Version:     0x10,
	Blocks: []product.Block{{
		Name: "Thermostat",
		Params: []product.Param{
			{Name: "setpoint", Text: "Setpoint after restart (°C)", Offset: 0, Bits: 8, Default: 21, Min: 5, Max: 30},
			{Name: "control", Text: "Control", Offset: 1, Bits: 8, Values: []product.Value{
				{Value: 0, Text: "Continuous (PI), 0-100 %"}, {Value: 1, Text: "Two-point, on/off"},
			}},
			{Name: "hysteresis", Text: "Two-point hysteresis (0.1 K)", Offset: 2, Bits: 8, Default: 5, Min: 1, Max: 20},
			{Name: "change", Text: "Send temperature on change of", Offset: 3, Bits: 8, Default: 2, Values: []product.Value{
				{Value: 0, Text: "Disabled"}, {Value: 1, Text: "0.1 K"}, {Value: 2, Text: "0.2 K"}, {Value: 5, Text: "0.5 K"},
			}},
			{Name: "cycle", Text: "Send temperature cyclically", Offset: 4, Bits: 8, Values: []product.Value{
				{Value: 0, Text: "Disabled"}, {Value: 1, Text: "Every minute"}, {Value: 5, Text: "Every 5 minutes"},
			}},
			{Name: "room", Text: "Simulated room", Offset: 5, Bits: 8, Default: 1, Values: rooms},
		},
		Objects: []product.Object{
			{Name: "Actual temperature", Function: "Temperature", DPT: "9.001", Flags: c | r | t},
			{Name: "Setpoint", Function: "Temperature", DPT: "9.001", Flags: c | r | w | t},
			{Name: "Heating output", Function: "Continuous control value", DPT: "5.001", Flags: c | r | t},
			{Name: "Heating on/off", Function: "Two-point control value", DPT: "1.001", Flags: c | r | t},
		},
	}},
}

// Heater is a heating actuator: a valve driven by a continuous or a switching control value.
var Heater = &product.Product{
	Name:        "Heating actuator 1-fold",
	OrderNumber: "TKGHA1",
	Application: 0x7405,
	Version:     0x10,
	Blocks: []product.Block{{
		Name: "Heating",
		Params: []product.Param{
			{Name: "power", Text: "Heating power", Offset: 0, Bits: 8, Default: 2, Values: []product.Value{
				{Value: 1, Text: "1000 W"}, {Value: 2, Text: "2000 W"}, {Value: 3, Text: "3000 W"},
			}},
			{Name: "room", Text: "Simulated room", Offset: 1, Bits: 8, Default: 1, Values: rooms},
		},
		Objects: []product.Object{
			{Name: "Control value", Function: "Valve position", DPT: "5.001", Flags: c | w},
			{Name: "Switching", Function: "On/Off", DPT: "1.001", Flags: c | w},
			{Name: "Status valve", Function: "Valve position", DPT: "5.001", Flags: c | r | t},
		},
	}},
}

// Products are the simulated device types by the names knxvbus uses for them.
var Products = map[string]*product.Product{
	"switch":     Switch,
	"dimmer":     Dimmer,
	"buttons":    Buttons,
	"thermostat": Thermostat,
	"heater":     Heater,
}
