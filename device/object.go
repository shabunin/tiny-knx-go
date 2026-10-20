package device

import (
	"github.com/shabunin/tiny-knx-go/apdu"
	"github.com/shabunin/tiny-knx-go/dpt"
)

// A Packer is a value that can be encoded for the bus. Every dpt type is one, also as a plain
// value such as dpt.DPT_1001(true).
type Packer interface {
	Pack() []byte
}

// An Object is a group object: a value of the device that is exchanged with other devices over
// group addresses. Its value is stored in the encoding of package dpt.
type Object struct {
	device   *Device
	index    int
	config   ObjectConfig
	value    []byte
	onChange func(*Object)
}

// Index returns the position of the object in the configuration.
func (o *Object) Index() int {
	return o.index
}

// Name returns the configured name.
func (o *Object) Name() string {
	return o.config.Name
}

// Flags returns the configured flags.
func (o *Object) Flags() Flags {
	return o.config.Flags
}

// Value returns a copy of the encoded value.
func (o *Object) Value() []byte {
	o.device.mu.Lock()
	defer o.device.mu.Unlock()

	return append([]byte(nil), o.value...)
}

// Get decodes the value, e.g. into a *dpt.DPT_1001.
func (o *Object) Get(value dpt.DatapointValue) error {
	return value.Unpack(o.Value())
}

// Set stores a new value, e.g. dpt.DPT_1001(true), and sends it if the object has the C and T
// flags.
func (o *Object) Set(value Packer) {
	o.SetValue(value.Pack())
}

// SetValue stores a new encoded value and sends it if the object has the C and T flags.
func (o *Object) SetValue(data []byte) {
	d := o.device

	d.mu.Lock()
	o.value = append(o.value[:0], data...)

	if o.has(FlagCommunicate|FlagTransmit) && len(o.config.Addresses) > 0 {
		d.sendGroup(o.config.Addresses[0], apdu.GroupValueWrite{Data: o.value})
	}

	d.mu.Unlock()
}

// Store stores a new value without sending it. Other devices can still read it.
func (o *Object) Store(value Packer) {
	d := o.device

	d.mu.Lock()
	o.value = append(o.value[:0], value.Pack()...)
	d.mu.Unlock()
}

// RequestRead asks the bus for the current value of the object's first group address. The answer
// updates the object if it has the U flag.
func (o *Object) RequestRead() {
	d := o.device

	d.mu.Lock()
	if o.has(FlagCommunicate) && len(o.config.Addresses) > 0 {
		d.sendGroup(o.config.Addresses[0], apdu.GroupValueRead{})
	}
	d.mu.Unlock()
}

// OnChange registers a function that is called when the value is changed from the bus. It runs on
// the device's goroutine; it may call methods of the object and the device.
func (o *Object) OnChange(f func(*Object)) {
	o.device.mu.Lock()
	o.onChange = f
	o.device.mu.Unlock()
}

func (o *Object) has(flags Flags) bool {
	return o.config.Flags&flags == flags
}

// receive stores a value from the bus. The device lock is held.
func (o *Object) receive(data []byte) {
	o.value = append(o.value[:0], data...)

	if f := o.onChange; f != nil {
		o.device.later(func() { f(o) })
	}
}
