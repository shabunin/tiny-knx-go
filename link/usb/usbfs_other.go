//go:build !linux

package usb

import "errors"

// ErrNoDevice is returned by OpenDevice when no KNX USB interface is found.
var ErrNoDevice = errors.New("usb: no KNX USB interface found")

// A Device is a KNX USB interface. OpenDevice is only implemented on Linux.
type Device struct{ DeviceInfo }

// DeviceInfo describes a KNX USB interface.
type DeviceInfo struct {
	Path         string
	Vendor       uint16
	Product      uint16
	Manufacturer string
	Name         string
	Serial       string
}

func (d *Device) Read(p []byte) (int, error)  { return 0, ErrNoDevice }
func (d *Device) Write(p []byte) (int, error) { return 0, ErrNoDevice }
func (d *Device) Close() error                { return nil }

// Devices lists KNX USB interfaces. It is only implemented on Linux.
func Devices(all bool) ([]DeviceInfo, error) { return nil, nil }

// OpenDevice opens a KNX USB interface. It is only implemented on Linux.
func OpenDevice(name string) (*Device, error) { return nil, ErrNoDevice }
