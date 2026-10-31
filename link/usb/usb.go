// Package usb connects to KNX USB interfaces (KNX 09_03, "KNX USB Interface"). They are HID
// devices that carry EMI messages in 64-byte reports: cEMI, EMI2 or EMI1, whichever the
// interface supports. The messages go through the same logic as those of serial modules (package
// ft12), so the Link that Open returns is an ft12.Link.
//
// On Linux, OpenDevice reaches the interface through usbfs and needs no hidraw driver; any other
// io.ReadWriteCloser that reads and writes one report at a time, such as a hidraw device, works
// too. The usbfs node of the interface is writable by root only; a udev rule opens it to a group:
//
//	SUBSYSTEM=="usb", ENV{DEVTYPE}=="usb_device", ATTR{idVendor}=="28c2", GROUP="plugdev", MODE="0660"
package usb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shabunin/tiny-knx-go/link"
	"github.com/shabunin/tiny-knx-go/link/ft12"
)

var (
	// ErrEMINotSupported is returned by Open for an interface that only speaks EMI1.
	ErrEMINotSupported = errors.New("usb: the interface supports neither cEMI nor EMI2")

	// ErrFeatureTimeout is returned when the interface does not answer a device feature request.
	ErrFeatureTimeout = errors.New("usb: no answer to the device feature request")

	// ErrTooLong is returned for a message that does not fit in a transfer.
	ErrTooLong = errors.New("usb: message too long")
)

// ReportSize is the size of a HID report of a KNX USB interface.
const ReportSize = 64

// The HID report header.
const (
	reportID     = 0x01
	reportHeader = 3 // report ID, packet info, data length

	packetStart   = 0x01
	packetEnd     = 0x02
	packetPartial = 0x04
)

// The transfer protocol header, at the start of a transfer.
const (
	transferVersion = 0x00
	transferHeader  = 8
)

// Protocols of a transfer.
const (
	protocolTunnel    = 0x01 // KNX tunnel: EMI messages
	protocolBusAccess = 0x0F // bus access server: device features
)

// EMI identifies the message format in a KNX tunnel transfer.
type EMI uint8

const (
	EMI1 EMI = 0x01
	EMI2 EMI = 0x02
	CEMI EMI = 0x03
)

func (e EMI) String() string {
	switch e {
	case EMI1:
		return "EMI1"
	case EMI2:
		return "EMI2"
	case CEMI:
		return "cEMI"
	default:
		return fmt.Sprintf("EMI(%d)", uint8(e))
	}
}

// Services of the bus access server.
const (
	serviceFeatureGet      = 0x01
	serviceFeatureResponse = 0x02
	serviceFeatureSet      = 0x03
	serviceFeatureInfo     = 0x04
)

// Feature is a device feature of the bus access server.
type Feature uint8

const (
	FeatureSupportedEMI  Feature = 0x01 // 2 bytes: bit 0 EMI1, bit 1 EMI2, bit 2 cEMI
	FeatureDescriptor    Feature = 0x02 // 2 bytes: device descriptor type 0 (mask version)
	FeatureBusConnection Feature = 0x03 // 1 byte: 1 if the bus is connected
	FeatureManufacturer  Feature = 0x04 // 2 bytes: KNX manufacturer code
	FeatureActiveEMI     Feature = 0x05 // 1 byte: the EMI of the tunnel
)

// A transfer is a message of the transfer protocol.
type transfer struct {
	protocol byte
	service  byte // EMI ID for the KNX tunnel, service ID for the bus access server
	body     []byte
}

// maxBody is the largest body a transfer may have: five reports.
const maxBody = 5*(ReportSize-reportHeader) - transferHeader

// reports splits a transfer into HID reports.
func (t transfer) reports() ([][]byte, error) {
	if len(t.body) > maxBody {
		return nil, ErrTooLong
	}

	data := make([]byte, transferHeader, transferHeader+len(t.body))
	data[0] = transferVersion
	data[1] = transferHeader
	binary.BigEndian.PutUint16(data[2:], uint16(len(t.body)))
	data[4] = t.protocol
	data[5] = t.service
	// The manufacturer code stays 0: no manufacturer specific protocol.
	data = append(data, t.body...)

	var reports [][]byte

	for seq := byte(1); len(data) > 0; seq++ {
		n := min(len(data), ReportSize-reportHeader)

		var kind byte
		if seq == 1 {
			kind |= packetStart
		}

		if n == len(data) {
			kind |= packetEnd
		}

		if kind != packetStart|packetEnd {
			kind |= packetPartial
		}

		report := make([]byte, ReportSize)
		report[0] = reportID
		report[1] = seq<<4 | kind
		report[2] = byte(n)
		copy(report[reportHeader:], data[:n])

		reports = append(reports, report)
		data = data[n:]
	}

	return reports, nil
}

// An assembler joins HID reports into transfers.
type assembler struct {
	data []byte
	seq  byte
}

// add takes a report and returns the transfer it completes, if any.
func (a *assembler) add(report []byte) (transfer, bool) {
	if len(report) < reportHeader || report[0] != reportID {
		return transfer{}, false
	}

	seq, kind := report[1]>>4, report[1]&0x0F
	n := int(report[2])

	if n > len(report)-reportHeader {
		a.data = nil
		return transfer{}, false
	}

	switch {
	case kind&packetStart != 0:
		a.data = append(a.data[:0], report[reportHeader:reportHeader+n]...)

	case a.data != nil && seq == a.seq+1:
		a.data = append(a.data, report[reportHeader:reportHeader+n]...)

	default:
		a.data = nil
		return transfer{}, false
	}

	a.seq = seq

	if kind&packetEnd == 0 {
		return transfer{}, false
	}

	data := a.data
	a.data = nil

	if len(data) < transferHeader || data[0] != transferVersion || int(data[1]) < transferHeader {
		return transfer{}, false
	}

	size := int(binary.BigEndian.Uint16(data[2:]))
	header := int(data[1])

	if len(data) < header+size {
		return transfer{}, false
	}

	return transfer{protocol: data[4], service: data[5], body: data[header : header+size]}, true
}

// A Conn carries EMI messages to and from a KNX USB interface. It is the Transport of the
// ft12.Link that Open returns.
type Conn struct {
	dev io.ReadWriteCloser
	emi atomic.Uint32 // EMI of sent messages

	writeMu sync.Mutex
	inbound chan []byte

	featureMu sync.Mutex // one device feature request at a time
	features  chan transfer
	connected chan bool // bus connection status from feature info

	done chan struct{}
	once sync.Once
	err  error
}

var _ ft12.Transport = (*Conn)(nil)

// NewConn starts reading reports from dev, which reads and writes one HID report at a time.
// Messages are sent in the given EMI.
func NewConn(dev io.ReadWriteCloser, emi EMI) *Conn {
	c := &Conn{
		dev:       dev,
		inbound:   make(chan []byte, 32),
		features:  make(chan transfer, 1),
		connected: make(chan bool, 1),
		done:      make(chan struct{}),
	}

	c.emi.Store(uint32(emi))

	go c.serve()

	return c
}

func (c *Conn) serve() {
	defer close(c.inbound)

	var a assembler

	report := make([]byte, ReportSize)

	for {
		n, err := c.dev.Read(report)
		if err != nil {
			c.terminate(err)
			return
		}

		t, ok := a.add(report[:n])
		if !ok {
			continue
		}

		switch {
		case t.protocol == protocolTunnel:
			select {
			case c.inbound <- t.body:
			case <-c.done:
				return
			}

		case t.protocol == protocolBusAccess && t.service == serviceFeatureResponse:
			select {
			case c.features <- t:
			default:
			}

		case t.protocol == protocolBusAccess && t.service == serviceFeatureInfo:
			if len(t.body) == 2 && Feature(t.body[0]) == FeatureBusConnection {
				select {
				case <-c.connected:
				default:
				}

				c.connected <- t.body[1] != 0
			}
		}
	}
}

func (c *Conn) terminate(err error) {
	c.once.Do(func() {
		c.err = err
		close(c.done)
	})
}

func (c *Conn) write(t transfer) error {
	reports, err := t.reports()
	if err != nil {
		return err
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	for _, report := range reports {
		select {
		case <-c.done:
			return link.ErrClosed
		default:
		}

		if _, err := c.dev.Write(report); err != nil {
			return err
		}
	}

	return nil
}

// Send sends an EMI message to the interface.
func (c *Conn) Send(payload []byte) error {
	return c.write(transfer{protocol: protocolTunnel, service: byte(c.emi.Load()), body: payload})
}

// Reset does nothing: a USB interface has no link layer to reset.
func (c *Conn) Reset() error { return nil }

// Inbound returns the EMI messages from the interface.
func (c *Conn) Inbound() <-chan []byte { return c.inbound }

// Resets returns a channel that never fires: a USB interface that restarts comes back as a new
// USB device.
func (c *Conn) Resets() <-chan struct{} { return nil }

// BusConnection returns changes of the bus connection that the interface reports.
func (c *Conn) BusConnection() <-chan bool { return c.connected }

// featureTimeout limits the wait for a device feature response.
const featureTimeout = time.Second

// Feature reads a device feature of the interface.
func (c *Conn) Feature(f Feature) ([]byte, error) {
	c.featureMu.Lock()
	defer c.featureMu.Unlock()

	select {
	case <-c.features:
	default:
	}

	if err := c.write(transfer{protocol: protocolBusAccess, service: serviceFeatureGet, body: []byte{byte(f)}}); err != nil {
		return nil, err
	}

	timer := time.NewTimer(featureTimeout)
	defer timer.Stop()

	for {
		select {
		case t := <-c.features:
			if len(t.body) > 0 && Feature(t.body[0]) == f {
				return t.body[1:], nil
			}

		case <-c.done:
			return nil, link.ErrClosed

		case <-timer.C:
			return nil, ErrFeatureTimeout
		}
	}
}

// SetFeature writes a device feature of the interface. The interface does not confirm it.
func (c *Conn) SetFeature(f Feature, value []byte) error {
	return c.write(transfer{protocol: protocolBusAccess, service: serviceFeatureSet, body: append([]byte{byte(f)}, value...)})
}

// Close closes the device.
func (c *Conn) Close() error {
	c.terminate(link.ErrClosed)
	return c.dev.Close()
}

// Err returns the error that terminated the connection.
func (c *Conn) Err() error {
	select {
	case <-c.done:
		return c.err
	default:
		return nil
	}
}

// Open switches the interface to cEMI if it supports it, or else to EMI2, and opens a Link on it.
// The device is closed with the Link.
func Open(dev io.ReadWriteCloser, config ft12.Config) (*ft12.Link, error) {
	probe := NewConn(dev, CEMI)

	// The first request after claiming the interface may go unanswered (seen on a Zennio).
	supported, err := probe.Feature(FeatureSupportedEMI)
	for try := 1; err == ErrFeatureTimeout && try < 3; try++ {
		supported, err = probe.Feature(FeatureSupportedEMI)
	}

	if err != nil {
		probe.Close()
		return nil, err
	}

	var bits uint16
	if len(supported) == 2 {
		bits = binary.BigEndian.Uint16(supported)
	}

	emi, protocol := CEMI, ft12.ProtocolCEMI

	switch {
	case bits&0x04 != 0:
	case bits&0x02 != 0:
		emi, protocol = EMI2, ft12.ProtocolEMI2
	default:
		probe.Close()
		return nil, fmt.Errorf("%w (% x)", ErrEMINotSupported, supported)
	}

	if err := probe.SetFeature(FeatureActiveEMI, []byte{byte(emi)}); err != nil {
		probe.Close()
		return nil, err
	}

	probe.emi.Store(uint32(emi))
	config.Protocol = protocol

	return ft12.OpenTransport(probe, config)
}
