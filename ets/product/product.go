// Package product describes the ETS product of a device programmed with package ets and writes its
// product database, the XML that a tool such as OpenKNXproducer converts into a .knxprod for ETS.
//
// The same description gives the firmware its group objects (Objects) and reads the parameters
// that ETS downloaded (Product.Value), so that the database and the device cannot disagree about
// object numbers or parameter offsets.
//
// The database follows the knx-demo-tp product of the thelsing stack: manufacturer 0x00FA, a System
// B device (mask version 0x07B0) whose parameters are one relative segment of the application
// program, and the RegistrationInfo that current ETS versions need to import it.
package product

import (
	"fmt"

	"github.com/shabunin/tiny-knx-go/device"
)

// Manufacturer is the manufacturer ID of the products: 0x00FA, which the KNX Association leaves to
// development and hobby products.
const Manufacturer = 0x00FA

// A Product is a device type in the ETS catalog with its application program.
type Product struct {
	Name        string // catalog name, e.g. "Switch actuator 2-fold"
	OrderNumber string // letters and digits only, it is part of IDs
	Application uint16 // application number, unique per product
	Version     uint8  // application version, shown by ETS as major.minor of its two nibbles
	Blocks      []Block
}

// A Block is a parameter page in ETS with its parameters and group objects, e.g. a channel.
type Block struct {
	Name    string
	Params  []Param
	Objects []Object
}

// A Param is a parameter stored in the parameter segment, big-endian.
type Param struct {
	Name    string // unique within the product
	Text    string // label in ETS
	Offset  int    // byte offset in the parameter segment
	Bits    int    // 8 or 16
	Default int

	// Values make the parameter a choice. Without them it is a number from Min to Max.
	Values   []Value
	Min, Max int
}

// A Value is a choice of an enumeration parameter.
type Value struct {
	Value int
	Text  string
}

// An Object is a group object. Objects are numbered in the order of the blocks, from 1, which
// makes object index i ComObject number i+1 as package ets expects.
type Object struct {
	Name     string // e.g. "A: Switch", unique within the product
	Function string // e.g. "On/Off"
	DPT      string // e.g. "1.001"
	Flags    device.Flags
}

// Objects returns the group objects for device.Config. Their flags and group addresses come from
// the ETS download.
func (p *Product) Objects() []device.ObjectConfig {
	var objects []device.ObjectConfig

	for _, b := range p.Blocks {
		for _, o := range b.Objects {
			objects = append(objects, device.ObjectConfig{Name: o.Name, DPT: o.DPT})
		}
	}

	return objects
}

// ProgramVersion returns PID_PROG_VERSION as ETS writes it for the application.
func (p *Product) ProgramVersion() [5]byte {
	return [5]byte{Manufacturer >> 8, Manufacturer & 0xff, byte(p.Application >> 8), byte(p.Application), p.Version}
}

// ParamSize returns the size of the parameter segment.
func (p *Product) ParamSize() int {
	size := 0

	for _, b := range p.Blocks {
		for _, param := range b.Params {
			size = max(size, param.Offset+param.Bits/8)
		}
	}

	return size
}

// Params gives access to the downloaded parameters; *ets.Device is one.
type Params interface {
	ParamBytes(offset, n int) []byte
}

// Value returns the value of the named parameter in params, or its default if params do not hold
// it (yet). It panics if the product has no such parameter.
func (p *Product) Value(params Params, name string) int {
	param := p.param(name)

	data := params.ParamBytes(param.Offset, param.Bits/8)
	if len(data) != param.Bits/8 {
		return param.Default
	}

	value := 0
	for _, b := range data {
		value = value<<8 | int(b)
	}

	return value
}

func (p *Product) param(name string) *Param {
	for i := range p.Blocks {
		for j := range p.Blocks[i].Params {
			if param := &p.Blocks[i].Params[j]; param.Name == name {
				return param
			}
		}
	}

	panic(fmt.Sprintf("product %s has no parameter %q", p.OrderNumber, name))
}

// Check reports mistakes in the description: overlapping or oversized parameters, defaults out of
// range, duplicate names and unknown DPTs.
func (p *Product) Check() error {
	if p.OrderNumber == "" || !alphanumeric(p.OrderNumber) {
		return fmt.Errorf("product %q: order number %q must be letters and digits", p.Name, p.OrderNumber)
	}

	used := map[int]string{}
	names := map[string]bool{}

	for _, b := range p.Blocks {
		for _, param := range b.Params {
			if names[param.Name] {
				return fmt.Errorf("%s: duplicate name %q", p.OrderNumber, param.Name)
			}

			names[param.Name] = true

			if param.Bits != 8 && param.Bits != 16 {
				return fmt.Errorf("%s: parameter %q has %d bits", p.OrderNumber, param.Name, param.Bits)
			}

			for i := param.Offset; i < param.Offset+param.Bits/8; i++ {
				if other, ok := used[i]; ok {
					return fmt.Errorf("%s: parameters %q and %q overlap", p.OrderNumber, other, param.Name)
				}

				used[i] = param.Name
			}

			if !param.allows(param.Default) {
				return fmt.Errorf("%s: default %d of %q is not allowed", p.OrderNumber, param.Default, param.Name)
			}
		}

		for _, o := range b.Objects {
			if names[o.Name] {
				return fmt.Errorf("%s: duplicate name %q", p.OrderNumber, o.Name)
			}

			names[o.Name] = true

			if _, ok := objectSizes[mainType(o.DPT)]; !ok {
				return fmt.Errorf("%s: object %q has unsupported DPT %q", p.OrderNumber, o.Name, o.DPT)
			}
		}
	}

	return nil
}

func (param *Param) allows(v int) bool {
	if len(param.Values) == 0 {
		return v >= param.Min && v <= param.Max
	}

	for _, value := range param.Values {
		if value.Value == v {
			return true
		}
	}

	return false
}

func alphanumeric(s string) bool {
	for _, c := range s {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return false
		}
	}

	return true
}
