// Package mock provides an in-memory KNX bus for tests.
package mock

import (
	"sync"

	"github.com/shabunin/tiny-knx-go/cemi"
	"github.com/shabunin/tiny-knx-go/link"
)

// A Bus connects Links like a twisted pair line: a frame sent by one link is received by all
// others.
type Bus struct {
	mu    sync.Mutex
	links []*Link

	// Drop, if set, is called for every frame and may discard it. Dropped frames are still
	// confirmed to the sender, like a frame that was lost after the bus acknowledged it.
	Drop func(from cemi.IndividualAddr, frame *cemi.LData) bool
}

// NewBus creates an empty bus.
func NewBus() *Bus {
	return &Bus{}
}

// A Link is a device attached to a Bus.
type Link struct {
	bus     *Bus
	addr    cemi.IndividualAddr
	inbound chan *cemi.LDataInd
	once    sync.Once
	closed  bool
	dropped int
}

var (
	_ link.Link          = (*Link)(nil)
	_ link.Addresser     = (*Link)(nil)
	_ link.AddressSetter = (*Link)(nil)
)

// Attach adds a device with the given individual address to the bus.
func (bus *Bus) Attach(addr cemi.IndividualAddr) *Link {
	l := &Link{bus: bus, addr: addr, inbound: make(chan *cemi.LDataInd, 64)}

	bus.mu.Lock()
	bus.links = append(bus.links, l)
	bus.mu.Unlock()

	return l
}

// Address returns the individual address of the link.
func (l *Link) Address() cemi.IndividualAddr {
	l.bus.mu.Lock()
	defer l.bus.mu.Unlock()

	return l.addr
}

// SetAddress changes the individual address of the link.
func (l *Link) SetAddress(addr cemi.IndividualAddr) error {
	l.bus.mu.Lock()
	l.addr = addr
	l.bus.mu.Unlock()

	return nil
}

// Send delivers the frame to all other links. Frames to an individual address are confirmed only
// if a link with that address is attached. A source address of 0.0.0 is replaced by the link's
// address, as a real interface does.
func (l *Link) Send(req *cemi.LDataReq) error {
	bus := l.bus

	bus.mu.Lock()
	defer bus.mu.Unlock()

	if l.closed {
		return link.ErrClosed
	}

	frame := req.LData
	if frame.Source == 0 {
		frame.Source = l.addr
	}

	reachable := frame.Control2.IsGroupAddr()

	for _, other := range bus.links {
		if other != l && !other.closed && !frame.Control2.IsGroupAddr() &&
			other.addr == cemi.IndividualAddr(frame.Destination) {
			reachable = true
		}
	}

	if !reachable {
		return link.ErrNotConfirmed
	}

	if bus.Drop != nil && bus.Drop(l.addr, &frame) {
		return nil
	}

	// Every receiver gets its own copy, decoded from the wire format.
	wire := make([]byte, cemi.Size(&cemi.LDataInd{LData: frame}))
	cemi.Pack(wire, &cemi.LDataInd{LData: frame})

	for _, other := range bus.links {
		if other == l || other.closed {
			continue
		}

		var msg cemi.Message
		if _, err := cemi.Unpack(wire, &msg); err != nil {
			return err
		}

		select {
		case other.inbound <- msg.(*cemi.LDataInd):
		default:
			other.dropped++
		}
	}

	return nil
}

// Dropped returns the number of frames this link missed because its inbound queue was full.
func (l *Link) Dropped() int {
	l.bus.mu.Lock()
	defer l.bus.mu.Unlock()

	return l.dropped
}

// Inbound returns the frames received from the bus.
func (l *Link) Inbound() <-chan *cemi.LDataInd {
	return l.inbound
}

// Close detaches the link from the bus.
func (l *Link) Close() error {
	l.once.Do(func() {
		l.bus.mu.Lock()
		l.closed = true
		close(l.inbound)
		l.bus.mu.Unlock()
	})

	return nil
}
