package knxip

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shabunin/tiny-knx-go/cemi"
	"github.com/shabunin/tiny-knx-go/knxnet"
	"github.com/shabunin/tiny-knx-go/link"
)

// RouterConfig configures a Router.
type RouterConfig struct {
	// Address is the individual address of the router participant. It replaces the source
	// address 0.0.0 of sent frames, and received frames with this source are ignored (multicast
	// loopback).
	Address cemi.IndividualAddr

	// Interval is the minimum time between two sent frames. KNXnet/IP routing allows at most 50
	// frames per second.
	Interval time.Duration

	// InboundQueue is the number of received frames that are buffered for the reader. When the
	// queue is full, new frames are dropped (see Dropped).
	InboundQueue int
}

// DefaultRouterConfig sends at most 50 frames per second.
var DefaultRouterConfig = RouterConfig{
	Interval:     20 * time.Millisecond,
	InboundQueue: 32,
}

// A Router is a link through KNXnet/IP routing: frames are sent to and received from a multicast
// group, usually 224.0.23.12:3671, where KNX IP routers forward them to and from their lines.
//
// Routing has no confirmations: Send returns once the frame is on the network. Whether a device
// received it is only known from the transport layer, e.g. its T_ACK.
type Router struct {
	config RouterConfig
	rx     net.PacketConn
	tx     net.PacketConn
	group  net.Addr

	sendMu    sync.Mutex
	lastSend  time.Time
	busyUntil atomic.Int64 // unix nanoseconds; set by ROUTING_BUSY

	inbound chan *cemi.LDataInd
	dropped atomic.Uint32
	lost    atomic.Uint32
	address atomic.Uint32
}

var (
	_ link.Link          = (*Router)(nil)
	_ link.Addresser     = (*Router)(nil)
	_ link.AddressSetter = (*Router)(nil)
)

// ListenRouter joins the standard routing multicast group on the given network interface (nil for
// the system default).
func ListenRouter(ifi *net.Interface, config RouterConfig) (*Router, error) {
	group := &net.UDPAddr{IP: net.IP(knxnet.RoutingGroup[:]), Port: knxnet.Port}

	rx, err := net.ListenMulticastUDP("udp4", ifi, group)
	if err != nil {
		return nil, err
	}

	// ListenMulticastUDP turns off multicast loopback, which would hide our frames from other
	// programs on this host, so frames are sent from a second socket. Bound to an address of the
	// interface, its multicast goes out there.
	var local *net.UDPAddr

	if ifi != nil {
		local = &net.UDPAddr{IP: interfaceIPv4(ifi)}
	}

	tx, err := net.ListenUDP("udp4", local)
	if err != nil {
		rx.Close()
		return nil, err
	}

	return newRouter(rx, tx, group, config), nil
}

func interfaceIPv4(ifi *net.Interface) net.IP {
	addrs, _ := ifi.Addrs()

	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && ipnet.IP.To4() != nil {
			return ipnet.IP
		}
	}

	return nil
}

// NewRouter runs a router on conn, sending to group. It takes ownership of conn.
func NewRouter(conn net.PacketConn, group net.Addr, config RouterConfig) *Router {
	return newRouter(conn, conn, group, config)
}

func newRouter(rx, tx net.PacketConn, group net.Addr, config RouterConfig) *Router {
	if config.Interval <= 0 {
		config.Interval = DefaultRouterConfig.Interval
	}

	if config.InboundQueue <= 0 {
		config.InboundQueue = DefaultRouterConfig.InboundQueue
	}

	r := &Router{
		config:  config,
		rx:      rx,
		tx:      tx,
		group:   group,
		inbound: make(chan *cemi.LDataInd, config.InboundQueue),
	}

	r.address.Store(uint32(config.Address))

	go r.read()

	return r
}

// Address returns the individual address of the router participant.
func (r *Router) Address() cemi.IndividualAddr {
	return cemi.IndividualAddr(r.address.Load())
}

// SetAddress changes the individual address.
func (r *Router) SetAddress(addr cemi.IndividualAddr) error {
	r.address.Store(uint32(addr))
	return nil
}

// Inbound returns the frames received from the network.
func (r *Router) Inbound() <-chan *cemi.LDataInd {
	return r.inbound
}

// Dropped returns the number of received frames that were dropped because the inbound queue was
// full.
func (r *Router) Dropped() uint32 {
	return r.dropped.Load()
}

// Lost returns the number of frames that routers reported lost (ROUTING_LOST_MESSAGE).
func (r *Router) Lost() uint32 {
	return r.lost.Load()
}

// Send sends the frame to the multicast group as L_Data.ind. It waits as long as a router asked
// to pause and keeps the minimum interval between frames.
func (r *Router) Send(req *cemi.LDataReq) error {
	ind := &cemi.LDataInd{LData: req.LData}
	if ind.Source == 0 {
		ind.Source = r.Address()
	}

	packet := knxnet.Encode(&knxnet.RoutingIndication{Message: ind})

	r.sendMu.Lock()
	defer r.sendMu.Unlock()

	wait := time.Until(r.lastSend.Add(r.config.Interval))
	if busy := time.Until(time.Unix(0, r.busyUntil.Load())); busy > wait {
		wait = busy
	}

	if wait > 0 {
		time.Sleep(wait)
	}

	r.lastSend = time.Now()

	if _, err := r.tx.WriteTo(packet, r.group); err != nil {
		if isClosed(err) {
			return link.ErrClosed
		}

		return err
	}

	return nil
}

// Close leaves the multicast group.
func (r *Router) Close() error {
	if r.tx != r.rx {
		r.tx.Close()
	}

	return r.rx.Close()
}

func (r *Router) read() {
	defer close(r.inbound)

	buffer := make([]byte, 512)

	for {
		n, _, err := r.rx.ReadFrom(buffer)
		if err != nil {
			if isClosed(err) {
				return
			}

			continue
		}

		s, err := knxnet.Decode(buffer[:n])
		if err != nil {
			continue
		}

		switch s := s.(type) {
		case *knxnet.RoutingIndication:
			ind, ok := s.Message.(*cemi.LDataInd)
			if !ok || ind.Source == r.Address() {
				continue
			}

			select {
			case r.inbound <- ind:
			default:
				r.dropped.Add(1)
			}

		case *knxnet.RoutingBusy:
			until := time.Now().Add(time.Duration(s.WaitTime) * time.Millisecond)
			if until.UnixNano() > r.busyUntil.Load() {
				r.busyUntil.Store(until.UnixNano())
			}

		case *knxnet.RoutingLostMessage:
			r.lost.Add(uint32(s.Lost))
		}
	}
}

func isClosed(err error) bool {
	return errors.Is(err, net.ErrClosed)
}
