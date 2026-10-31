package usb

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

// ErrNoDevice is returned by OpenDevice when no KNX USB interface is found.
var ErrNoDevice = errors.New("usb: no KNX USB interface found")

// DeviceInfo describes a KNX USB interface.
type DeviceInfo struct {
	Path         string // usbfs node, /dev/bus/usb/BBB/DDD
	Vendor       uint16
	Product      uint16
	Manufacturer string
	Name         string
	Serial       string

	iface   int
	in, out byte // interrupt endpoints
}

func (d DeviceInfo) String() string {
	return fmt.Sprintf("%s %04x:%04x %s %s", d.Path, d.Vendor, d.Product, d.Manufacturer, d.Name)
}

const sysfsDevices = "/sys/bus/usb/devices"

// Devices lists the HID devices with an interrupt IN and OUT endpoint of 64 bytes, the shape of a
// KNX USB interface. If all is false, only those whose manufacturer or product name mentions KNX
// or EIB are listed.
func Devices(all bool) ([]DeviceInfo, error) {
	ifaces, err := filepath.Glob(filepath.Join(sysfsDevices, "*:*"))
	if err != nil {
		return nil, err
	}

	var devices []DeviceInfo

	for _, dir := range ifaces {
		if sysfsString(dir, "bInterfaceClass") != "03" { // HID
			continue
		}

		d := DeviceInfo{iface: int(sysfsHex(dir, "bInterfaceNumber"))}

		eps, _ := filepath.Glob(filepath.Join(dir, "ep_*"))
		for _, ep := range eps {
			if sysfsString(ep, "type") != "Interrupt" || sysfsHex(ep, "wMaxPacketSize") != ReportSize {
				continue
			}

			addr := byte(sysfsHex(ep, "bEndpointAddress"))

			if sysfsString(ep, "direction") == "in" {
				d.in = addr
			} else {
				d.out = addr
			}
		}

		if d.in == 0 || d.out == 0 {
			continue
		}

		// The interface 4-1:1.0 belongs to the device 4-1.
		dev := filepath.Join(sysfsDevices, strings.SplitN(filepath.Base(dir), ":", 2)[0])
		d.Vendor = uint16(sysfsHex(dev, "idVendor"))
		d.Product = uint16(sysfsHex(dev, "idProduct"))
		d.Manufacturer = sysfsString(dev, "manufacturer")
		d.Name = sysfsString(dev, "product")
		d.Serial = sysfsString(dev, "serial")
		if bus, err := strconv.Atoi(sysfsString(dev, "busnum")); err == nil {
			if num, err := strconv.Atoi(sysfsString(dev, "devnum")); err == nil {
				d.Path = fmt.Sprintf("/dev/bus/usb/%03d/%03d", bus, num)
			}
		}

		names := strings.ToUpper(d.Manufacturer + " " + d.Name)
		if all || strings.Contains(names, "KNX") || strings.Contains(names, "EIB") {
			devices = append(devices, d)
		}
	}

	return devices, nil
}

func sysfsString(dir, name string) string {
	data, _ := os.ReadFile(filepath.Join(dir, name))
	return strings.TrimSpace(string(data))
}

func sysfsHex(dir, name string) uint64 {
	n, _ := strconv.ParseUint(sysfsString(dir, name), 16, 32)
	return n
}

// OpenDevice opens a KNX USB interface through usbfs, detaching the kernel's HID driver from it
// until Close. name selects the interface: "" for the first one whose name mentions KNX or EIB,
// "VVVV:PPPP" for a vendor and product ID in hex, or a usbfs node such as /dev/bus/usb/004/002.
// The process needs write access to the usbfs node.
func OpenDevice(name string) (*Device, error) {
	devices, err := Devices(name != "")
	if err != nil {
		return nil, err
	}

	for _, d := range devices {
		if name == "" || name == d.Path || name == fmt.Sprintf("%04x:%04x", d.Vendor, d.Product) {
			return openDevice(d)
		}
	}

	if name != "" {
		return nil, fmt.Errorf("%w: %s", ErrNoDevice, name)
	}

	return nil, ErrNoDevice
}

// usbfs ioctls (linux/usbdevice_fs.h).
const (
	iocNone  = 0
	iocWrite = 1
	iocRead  = 2
)

func ioc(dir, nr, size uintptr) uintptr { return dir<<30 | size<<16 | 'U'<<8 | nr }

type bulkTransfer struct {
	ep, len, timeout uint32 // timeout in ms
	data             unsafe.Pointer
}

type usbIoctl struct {
	ifno, code int32
	data       unsafe.Pointer
}

var (
	usbdevfsBulk             = ioc(iocRead|iocWrite, 2, unsafe.Sizeof(bulkTransfer{}))
	usbdevfsClaimInterface   = ioc(iocRead, 15, 4)
	usbdevfsReleaseInterface = ioc(iocRead, 16, 4)
	usbdevfsIoctl            = ioc(iocRead|iocWrite, 18, unsafe.Sizeof(usbIoctl{}))
	usbdevfsDisconnect       = ioc(iocNone, 22, 0)
	usbdevfsConnect          = ioc(iocNone, 23, 0)
)

func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(arg)); errno != 0 {
		return errno
	}

	return nil
}

// A Device is a KNX USB interface opened through usbfs. Read and Write transfer one report each.
type Device struct {
	DeviceInfo

	fd int

	mu       sync.RWMutex // Close waits for transfers in progress
	closed   bool
	detached bool // the kernel driver was detached and is reattached on Close
}

// readTimeout bounds a read transfer, so that Close need not wait long for a Read in progress.
const readTimeout = 250

// writeTimeout bounds a write transfer, in ms.
const writeTimeout = 1000

func openDevice(info DeviceInfo) (*Device, error) {
	fd, err := syscall.Open(info.Path, syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: info.Path, Err: err}
	}

	d := &Device{DeviceInfo: info, fd: fd}

	cmd := usbIoctl{ifno: int32(info.iface), code: int32(usbdevfsDisconnect)}

	switch err := ioctl(fd, usbdevfsIoctl, unsafe.Pointer(&cmd)); err {
	case nil:
		d.detached = true
	case syscall.ENODATA: // no driver bound
	default:
		syscall.Close(fd)
		return nil, fmt.Errorf("usb: detach the kernel driver from %s: %w", info.Path, err)
	}

	iface := uint32(info.iface)
	if err := ioctl(fd, usbdevfsClaimInterface, unsafe.Pointer(&iface)); err != nil {
		d.reattach()
		syscall.Close(fd)

		return nil, fmt.Errorf("usb: claim %s: %w", info.Path, err)
	}

	return d, nil
}

func (d *Device) transfer(ep byte, p []byte, timeout uint32) (int, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if d.closed {
		return 0, os.ErrClosed
	}

	t := bulkTransfer{ep: uint32(ep), len: uint32(len(p)), timeout: timeout, data: unsafe.Pointer(&p[0])}

	n, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(d.fd), usbdevfsBulk, uintptr(unsafe.Pointer(&t)))
	if errno != 0 {
		return 0, errno
	}

	return int(n), nil
}

// Read waits for a report from the interface.
func (d *Device) Read(p []byte) (int, error) {
	for {
		n, err := d.transfer(d.in, p, readTimeout)

		switch err {
		case syscall.ETIMEDOUT:
			continue
		case syscall.ENODEV:
			return 0, fmt.Errorf("usb: %s unplugged", d.Path)
		}

		return n, err
	}
}

// Write sends a report to the interface.
func (d *Device) Write(p []byte) (int, error) {
	n, err := d.transfer(d.out, p, writeTimeout)
	if err != nil {
		return n, fmt.Errorf("usb: write to %s: %w", d.Path, err)
	}

	return n, nil
}

func (d *Device) reattach() {
	if d.detached {
		cmd := usbIoctl{ifno: int32(d.iface), code: int32(usbdevfsConnect)}
		ioctl(d.fd, usbdevfsIoctl, unsafe.Pointer(&cmd))
	}
}

// Close releases the interface and gives it back to the kernel's HID driver.
func (d *Device) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.closed {
		return nil
	}

	d.closed = true

	iface := uint32(d.iface)
	ioctl(d.fd, usbdevfsReleaseInterface, unsafe.Pointer(&iface))
	d.reattach()

	return syscall.Close(d.fd)
}
