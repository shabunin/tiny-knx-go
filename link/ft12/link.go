package ft12

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shabunin/tiny-knx-go/cemi"
	"github.com/shabunin/tiny-knx-go/link"
)

// ErrPropertyFailed is returned when the module rejects a property access.
var ErrPropertyFailed = errors.New("ft12: property access rejected by module")

// Config configures a Link.
type Config struct {
	Conn ConnConfig

	// Protocol is the message format of the module. ProtocolAuto, the zero value, detects it.
	Protocol Protocol

	// ConfirmTimeout is how long to wait for the L_Data.con of a sent frame. The module only
	// confirms after the frame went out on the bus, including bus-level repetitions.
	ConfirmTimeout time.Duration

	// InboundQueue is the number of received frames that are buffered for the reader. When the
	// queue is full, new frames are dropped (see Link.Dropped) instead of stalling the serial
	// connection.
	InboundQueue int
}

// DefaultConfig works with Weinzierl BAOS modules.
var DefaultConfig = Config{
	Conn:           DefaultConnConfig,
	ConfirmTimeout: 3 * time.Second,
	InboundQueue:   32,
}

func (cfg Config) withDefaults() Config {
	cfg.Conn = cfg.Conn.withDefaults()

	if cfg.ConfirmTimeout <= 0 {
		cfg.ConfirmTimeout = DefaultConfig.ConfirmTimeout
	}

	if cfg.InboundQueue <= 0 {
		cfg.InboundQueue = DefaultConfig.InboundQueue
	}

	return cfg
}

// A Link exchanges frames with a module that has been switched to data link layer mode: a BAOS
// module (e.g. kBerry) in cEMI, or an older EMI2 interface. Frames of EMI2 modules are converted
// to and from cEMI. Serial settings for BAOS modules are 19200 baud, 8 data bits, even parity,
// 1 stop bit.
type Link struct {
	conn     Transport
	config   Config
	protocol atomic.Uint32 // Protocol, ProtocolAuto until detected

	sendMu sync.Mutex // one request awaiting confirmation at a time
	cons   chan cemi.Message
	props  chan cemi.Message
	idents chan []byte // PEI_Identify.con of EMI2 modules

	inbound chan *cemi.LDataInd
	dropped atomic.Uint32
	address atomic.Uint32 // learned from L_Data.con

	// The communication mode of a cEMI module before Open, restored by Close: BAOS modules run
	// their ObjectServer (package baos) in CommModeObjectServer.
	restoreMode    atomic.Uint32
	restoreModeSet atomic.Bool
}

var (
	_ link.Link          = (*Link)(nil)
	_ link.Addresser     = (*Link)(nil)
	_ link.AddressSetter = (*Link)(nil)
)

// A Transport carries EMI messages between the Link and a module. Conn carries them in FT1.2
// frames over a serial port; package usb carries them in HID reports.
type Transport interface {
	// Reset resets the connection to the module.
	Reset() error

	// Send sends a message to the module.
	Send(payload []byte) error

	// Inbound returns the messages from the module. It is closed when the transport terminates.
	Inbound() <-chan []byte

	// Resets signals that the module restarted.
	Resets() <-chan struct{}

	Close() error
}

// Open resets the module behind port and switches it to data link layer mode.
func Open(port io.ReadWriter, config Config) (*Link, error) {
	config = config.withDefaults()

	return OpenTransport(NewConn(port, config.Conn), config)
}

// OpenTransport resets the module behind a transport and switches it to data link layer mode. The
// transport is closed with the Link. Config.Conn is not used.
func OpenTransport(t Transport, config Config) (*Link, error) {
	config = config.withDefaults()

	l := &Link{
		conn:    t,
		config:  config,
		cons:    make(chan cemi.Message, 1),
		props:   make(chan cemi.Message, 1),
		idents:  make(chan []byte, 1),
		inbound: make(chan *cemi.LDataInd, config.InboundQueue),
	}

	l.protocol.Store(uint32(config.Protocol))

	go l.serve()

	if err := l.init(); err != nil {
		l.Close()
		return nil, err
	}

	return l, nil
}

func (l *Link) init() error {
	if err := l.conn.Reset(); err != nil {
		return err
	}

	switch l.Protocol() {
	case ProtocolCEMI:
		l.saveCommMode(l.config.ConfirmTimeout)
		return l.switchCEMI(l.config.ConfirmTimeout)

	case ProtocolEMI2:
		return l.switchEMI2()
	}

	// A cEMI module answers property requests at once; an EMI2 module ignores them.
	timeout := min(l.config.ConfirmTimeout, detectTimeout)

	if l.saveCommMode(timeout) {
		l.protocol.Store(uint32(ProtocolCEMI))
		return l.switchCEMI(l.config.ConfirmTimeout)
	}

	if _, err := l.identify(timeout); err != nil {
		return err
	}

	l.protocol.Store(uint32(ProtocolEMI2))

	return l.switchEMI2()
}

// closeTimeout limits the wait for the module to confirm the mode it returns to.
const closeTimeout = time.Second

// detectTimeout limits the wait for an answer while the protocol of the module is not known.
const detectTimeout = time.Second

func (l *Link) switchCEMI(timeout time.Duration) error {
	l.sendMu.Lock()
	defer l.sendMu.Unlock()

	return l.writeProperty(cemi.ObjectTypeCEMIServer, 1, cemi.PIDCommMode, 1,
		[]byte{cemi.CommModeDataLinkLayer}, timeout)
}

// saveCommMode remembers the communication mode of a cEMI module, once, for Close. It reports
// whether the module answered, i.e. whether it is a cEMI module.
func (l *Link) saveCommMode(timeout time.Duration) bool {
	data, err := l.readProperty(cemi.ObjectTypeCEMIServer, 1, cemi.PIDCommMode, 1, 1, timeout)

	if err == nil && len(data) == 1 && data[0] != cemi.CommModeDataLinkLayer && !l.restoreModeSet.Load() {
		l.restoreMode.Store(uint32(data[0]))
		l.restoreModeSet.Store(true)
	}

	return err == nil || err == ErrPropertyFailed
}

// switchEMI2 switches an EMI2 module to data link layer mode. The module does not confirm it.
func (l *Link) switchEMI2() error {
	return l.send(emiSwitchLinkLayer)
}

// Protocol returns the message format of the module.
func (l *Link) Protocol() Protocol {
	return Protocol(l.protocol.Load())
}

// Send transmits the frame and waits for its L_Data.con.
func (l *Link) Send(req *cemi.LDataReq) error {
	l.sendMu.Lock()
	defer l.sendMu.Unlock()

	frame := make([]byte, cemi.Size(req))
	cemi.Pack(frame, req)

	if l.Protocol() == ProtocolEMI2 {
		var err error
		if frame, err = emiFromCEMI(frame); err != nil {
			return err
		}
	}

	con, err := request(l, frame, l.cons, l.config.ConfirmTimeout, func(msg cemi.Message) bool {
		_, ok := msg.(*cemi.LDataCon)
		return ok
	})
	if err != nil {
		return err
	}

	if con.(*cemi.LDataCon).Control1&cemi.Control1HasError != 0 {
		return link.ErrNotConfirmed
	}

	return nil
}

// ReadProperty reads count elements of a property of the module, starting at index start. EMI2
// modules have no properties.
func (l *Link) ReadProperty(
	objectType uint16, instance, propertyID uint8, start uint16, count uint8,
) ([]byte, error) {
	if l.Protocol() == ProtocolEMI2 {
		return nil, ErrNotSupported
	}

	return l.readProperty(objectType, instance, propertyID, start, count, l.config.ConfirmTimeout)
}

func (l *Link) readProperty(
	objectType uint16, instance, propertyID uint8, start uint16, count uint8, timeout time.Duration,
) ([]byte, error) {
	l.sendMu.Lock()
	defer l.sendMu.Unlock()

	req := &cemi.MPropReadReq{MProp: cemi.MProp{
		ObjectType: objectType, Instance: instance, PropertyID: propertyID,
		Count: count, Start: start,
	}}

	msg, err := request(l, packed(req), l.props, timeout, func(msg cemi.Message) bool {
		con, ok := msg.(*cemi.MPropReadCon)
		return ok && sameProperty(&con.MProp, &req.MProp)
	})
	if err != nil {
		return nil, err
	}

	con := msg.(*cemi.MPropReadCon)
	if con.Failed() {
		return nil, ErrPropertyFailed
	}

	return con.Data, nil
}

// WriteProperty writes elements to a property of the module, starting at index start. EMI2
// modules have no properties.
func (l *Link) WriteProperty(
	objectType uint16, instance, propertyID uint8, start uint16, data []byte,
) error {
	if l.Protocol() == ProtocolEMI2 {
		return ErrNotSupported
	}

	l.sendMu.Lock()
	defer l.sendMu.Unlock()

	return l.writeProperty(objectType, instance, propertyID, start, data, l.config.ConfirmTimeout)
}

func (l *Link) writeProperty(
	objectType uint16, instance, propertyID uint8, start uint16, data []byte, timeout time.Duration,
) error {
	req := &cemi.MPropWriteReq{MProp: cemi.MProp{
		ObjectType: objectType, Instance: instance, PropertyID: propertyID,
		Count: 1, Start: start, Data: data,
	}}

	msg, err := request(l, packed(req), l.props, timeout, func(msg cemi.Message) bool {
		con, ok := msg.(*cemi.MPropWriteCon)
		return ok && sameProperty(&con.MProp, &req.MProp)
	})
	if err != nil {
		return err
	}

	if msg.(*cemi.MPropWriteCon).Failed() {
		return ErrPropertyFailed
	}

	return nil
}

func sameProperty(a, b *cemi.MProp) bool {
	return a.ObjectType == b.ObjectType && a.Instance == b.Instance && a.PropertyID == b.PropertyID
}

func packed(msg cemi.Message) []byte {
	frame := make([]byte, cemi.Size(msg))
	cemi.Pack(frame, msg)

	return frame
}

// send sends a payload to the module.
func (l *Link) send(payload []byte) error {
	if err := l.conn.Send(payload); err != nil {
		if err == ErrClosed {
			return link.ErrClosed
		}

		return err
	}

	return nil
}

// request sends a payload and waits for a matching confirmation on the given channel.
func request[T any](
	l *Link, payload []byte, confirmations chan T, timeout time.Duration, matches func(T) bool,
) (T, error) {
	var none T

	// Discard a confirmation that arrived after its request timed out.
	select {
	case <-confirmations:
	default:
	}

	if err := l.send(payload); err != nil {
		return none, err
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case con, open := <-confirmations:
			if !open {
				return none, link.ErrClosed
			}

			if matches(con) {
				return con, nil
			}

		case <-timer.C:
			return none, link.ErrConfirmTimeout
		}
	}
}

// identify asks an EMI2 module for its individual address and serial number.
func (l *Link) identify(timeout time.Duration) ([]byte, error) {
	l.sendMu.Lock()
	defer l.sendMu.Unlock()

	return request(l, []byte{emiPEIIdentifyReq}, l.idents, timeout, func([]byte) bool { return true })
}

// Address returns the individual address of the module. It is learned from the first confirmed
// frame and is zero until then.
func (l *Link) Address() cemi.IndividualAddr {
	return cemi.IndividualAddr(l.address.Load())
}

// SetAddress changes the individual address of the module. The module only acknowledges and
// forwards point-to-point frames for its own address, so a device that uses the link must have the
// same address. The address is written only if it differs, to spare the module's memory.
func (l *Link) SetAddress(addr cemi.IndividualAddr) error {
	current, err := l.ReadAddress()
	if err != nil {
		return err
	}

	if current != addr && l.Protocol() == ProtocolEMI2 {
		// PC_SetValue.req writes the memory of the module; there is no confirmation.
		err := l.send([]byte{emiPCSetValueReq, 2, emiAddressMemory >> 8, emiAddressMemory & 0xff,
			byte(addr >> 8), byte(addr)})
		if err != nil {
			return err
		}

		if current, err = l.ReadAddress(); err != nil {
			return err
		}

		if current != addr {
			return ErrAddressIgnored
		}
	} else if current != addr {
		err := l.WriteProperty(cemi.ObjectTypeDevice, 1, cemi.PIDSubnetAddr, 1, []byte{byte(addr >> 8)})
		if err != nil {
			return err
		}

		err = l.WriteProperty(cemi.ObjectTypeDevice, 1, cemi.PIDDeviceAddr, 1, []byte{byte(addr)})
		if err != nil {
			return err
		}
	}

	l.address.Store(uint32(addr))

	return nil
}

// ReadAddress reads the individual address from the module's device object, or with
// PEI_Identify from an EMI2 module.
func (l *Link) ReadAddress() (cemi.IndividualAddr, error) {
	if l.Protocol() == ProtocolEMI2 {
		ident, err := l.identify(l.config.ConfirmTimeout)
		if err != nil {
			return 0, err
		}

		if len(ident) < 3 {
			return 0, ErrPropertyFailed
		}

		addr := cemi.IndividualAddr(ident[1])<<8 | cemi.IndividualAddr(ident[2])
		l.address.Store(uint32(addr))

		return addr, nil
	}

	subnet, err := l.ReadProperty(cemi.ObjectTypeDevice, 1, cemi.PIDSubnetAddr, 1, 1)
	if err != nil {
		return 0, err
	}

	device, err := l.ReadProperty(cemi.ObjectTypeDevice, 1, cemi.PIDDeviceAddr, 1, 1)
	if err != nil {
		return 0, err
	}

	if len(subnet) != 1 || len(device) != 1 {
		return 0, ErrPropertyFailed
	}

	addr := cemi.IndividualAddr(subnet[0])<<8 | cemi.IndividualAddr(device[0])
	l.address.Store(uint32(addr))

	return addr, nil
}

// SerialNumber reads the serial number from the module's device object, or with PEI_Identify from
// an EMI2 module.
func (l *Link) SerialNumber() ([6]byte, error) {
	var serial [6]byte

	if l.Protocol() == ProtocolEMI2 {
		ident, err := l.identify(l.config.ConfirmTimeout)
		if err != nil {
			return serial, err
		}

		if len(ident) < 9 {
			return serial, ErrPropertyFailed
		}

		copy(serial[:], ident[3:])

		return serial, nil
	}

	data, err := l.ReadProperty(cemi.ObjectTypeDevice, 1, cemi.PIDSerialNumber, 1, 1)
	if err != nil {
		return serial, err
	}

	if len(data) != len(serial) {
		return serial, ErrPropertyFailed
	}

	copy(serial[:], data)

	return serial, nil
}

// Inbound returns the frames received from the bus.
func (l *Link) Inbound() <-chan *cemi.LDataInd {
	return l.inbound
}

// Dropped returns the number of received frames that were dropped because the inbound queue was
// full.
func (l *Link) Dropped() uint32 {
	return l.dropped.Load()
}

// Close terminates the link and closes the port if it implements io.Closer. The module goes back to
// the mode it was in before Open, in which a BAOS module runs its own application again: an EMI2
// module to normal mode, a cEMI module to its former communication mode.
func (l *Link) Close() error {
	switch l.Protocol() {
	case ProtocolEMI2:
		l.send(emiSwitchNormal)

	case ProtocolCEMI:
		if l.restoreModeSet.Load() {
			l.sendMu.Lock()
			l.writeProperty(cemi.ObjectTypeCEMIServer, 1, cemi.PIDCommMode, 1,
				[]byte{byte(l.restoreMode.Load())}, closeTimeout)
			l.sendMu.Unlock()
		}
	}

	return l.conn.Close()
}

func (l *Link) serve() {
	defer close(l.inbound)
	defer close(l.idents)
	defer close(l.props)
	defer close(l.cons)

	for {
		select {
		case payload, open := <-l.conn.Inbound():
			if !open {
				return
			}

			l.dispatch(payload)

		case <-l.conn.Resets():
			// The module restarted and is back in its default mode.
			go l.init()
		}
	}
}

func (l *Link) dispatch(payload []byte) {
	if len(payload) == 0 {
		return
	}

	if payload[0] == emiPEIIdentifyCon {
		offer(l.idents, payload)
		return
	}

	switch l.Protocol() {
	case ProtocolEMI2:
		var ok bool
		if payload, ok = emiToCEMI(payload); !ok {
			return
		}

	case ProtocolAuto:
		// Until the protocol is known, only the answers to the detection are understood.
		if code := cemi.MessageCode(payload[0]); code != cemi.MPropReadConCode && code != cemi.MPropWriteConCode {
			return
		}
	}

	// In BAOS (object server) mode the module sends frames starting with 0xF0, which is also the
	// cEMI code of M_Reset.ind. Those are not for us.
	if payload[0] == byte(cemi.MResetIndCode) && len(payload) > 1 {
		return
	}

	var msg cemi.Message
	if _, err := cemi.Unpack(payload, &msg); err != nil {
		return
	}

	switch msg := msg.(type) {
	case *cemi.LDataInd:
		select {
		case l.inbound <- msg:
		default:
			l.dropped.Add(1)
		}

	case *cemi.LDataCon:
		// The module fills in its own address as the source of our frames.
		if msg.Source != 0 {
			l.address.Store(uint32(msg.Source))
		}

		offer[cemi.Message](l.cons, msg)

	case *cemi.MPropReadCon, *cemi.MPropWriteCon:
		offer[cemi.Message](l.props, msg)

	case *cemi.MResetInd:
		go l.init()
	}
}

// offer replaces a pending, unclaimed confirmation with the new one.
func offer[T any](ch chan T, msg T) {
	for {
		select {
		case ch <- msg:
			return
		default:
		}

		select {
		case <-ch:
		default:
		}
	}
}
