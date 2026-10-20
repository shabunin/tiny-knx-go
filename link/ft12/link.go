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

// A Link exchanges cEMI frames with a BAOS module (e.g. kBerry) that has been switched to data link
// layer mode. Serial settings for BAOS modules are 19200 baud, 8 data bits, even parity, 1 stop bit.
type Link struct {
	conn   *Conn
	config Config

	sendMu sync.Mutex // one request awaiting confirmation at a time
	cons   chan cemi.Message
	props  chan cemi.Message

	inbound chan *cemi.LDataInd
	dropped atomic.Uint32
	address atomic.Uint32 // learned from L_Data.con
}

var (
	_ link.Link          = (*Link)(nil)
	_ link.Addresser     = (*Link)(nil)
	_ link.AddressSetter = (*Link)(nil)
)

// Open resets the module behind port and switches it to data link layer mode.
func Open(port io.ReadWriter, config Config) (*Link, error) {
	config = config.withDefaults()

	l := &Link{
		conn:    NewConn(port, config.Conn),
		config:  config,
		cons:    make(chan cemi.Message, 1),
		props:   make(chan cemi.Message, 1),
		inbound: make(chan *cemi.LDataInd, config.InboundQueue),
	}

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

	return l.WriteProperty(
		cemi.ObjectTypeCEMIServer, 1, cemi.PIDCommMode, 1, []byte{cemi.CommModeDataLinkLayer},
	)
}

// Send transmits the frame and waits for its L_Data.con.
func (l *Link) Send(req *cemi.LDataReq) error {
	l.sendMu.Lock()
	defer l.sendMu.Unlock()

	con, err := l.request(req, l.cons, func(msg cemi.Message) bool {
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

// ReadProperty reads count elements of a property of the module, starting at index start.
func (l *Link) ReadProperty(
	objectType uint16, instance, propertyID uint8, start uint16, count uint8,
) ([]byte, error) {
	l.sendMu.Lock()
	defer l.sendMu.Unlock()

	req := &cemi.MPropReadReq{MProp: cemi.MProp{
		ObjectType: objectType, Instance: instance, PropertyID: propertyID,
		Count: count, Start: start,
	}}

	msg, err := l.request(req, l.props, func(msg cemi.Message) bool {
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

// WriteProperty writes elements to a property of the module, starting at index start.
func (l *Link) WriteProperty(
	objectType uint16, instance, propertyID uint8, start uint16, data []byte,
) error {
	l.sendMu.Lock()
	defer l.sendMu.Unlock()

	req := &cemi.MPropWriteReq{MProp: cemi.MProp{
		ObjectType: objectType, Instance: instance, PropertyID: propertyID,
		Count: 1, Start: start, Data: data,
	}}

	msg, err := l.request(req, l.props, func(msg cemi.Message) bool {
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

// request sends msg and waits for a matching confirmation on the given channel.
func (l *Link) request(
	msg cemi.Message, confirmations chan cemi.Message, matches func(cemi.Message) bool,
) (cemi.Message, error) {
	// Discard a confirmation that arrived after its request timed out.
	select {
	case <-confirmations:
	default:
	}

	frame := make([]byte, cemi.Size(msg))
	cemi.Pack(frame, msg)

	if err := l.conn.Send(frame); err != nil {
		if err == ErrClosed {
			return nil, link.ErrClosed
		}

		return nil, err
	}

	timeout := time.NewTimer(l.config.ConfirmTimeout)
	defer timeout.Stop()

	for {
		select {
		case con, open := <-confirmations:
			if !open {
				return nil, link.ErrClosed
			}

			if matches(con) {
				return con, nil
			}

		case <-timeout.C:
			return nil, link.ErrConfirmTimeout
		}
	}
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

	if current != addr {
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

// ReadAddress reads the individual address from the module's device object.
func (l *Link) ReadAddress() (cemi.IndividualAddr, error) {
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

	return cemi.IndividualAddr(subnet[0])<<8 | cemi.IndividualAddr(device[0]), nil
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

// Close terminates the link and closes the port if it implements io.Closer.
func (l *Link) Close() error {
	return l.conn.Close()
}

func (l *Link) serve() {
	defer close(l.inbound)
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
	// In BAOS (object server) mode the module sends frames starting with 0xF0, which is also the
	// cEMI code of M_Reset.ind. Those are not for us.
	if len(payload) == 0 || (payload[0] == byte(cemi.MResetIndCode) && len(payload) > 1) {
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

		offer(l.cons, msg)

	case *cemi.MPropReadCon, *cemi.MPropWriteCon:
		offer(l.props, msg)

	case *cemi.MResetInd:
		go l.init()
	}
}

// offer replaces a pending, unclaimed confirmation with the new one.
func offer(ch chan cemi.Message, msg cemi.Message) {
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
