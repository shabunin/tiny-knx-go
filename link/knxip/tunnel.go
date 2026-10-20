// Package knxip connects to KNX over IP: as a tunnelling client of a KNXnet/IP interface or router
// (Tunnel), or as a KNXnet/IP routing participant on the multicast group (Router). It uses package
// net; the protocol itself is in package knxnet.
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

// ErrDisconnected is returned by Tunnel.Send while the tunnel is down and reconnecting.
var ErrDisconnected = errors.New("knxip: tunnel disconnected")

// TunnelConfig configures a Tunnel.
type TunnelConfig struct {
	Tunnel knxnet.TunnelConfig

	// ConfirmTimeout is how long to wait for the L_Data.con of a sent frame after the server
	// acknowledged it.
	ConfirmTimeout time.Duration

	// ReconnectDelay is the pause before connecting again after the connection was lost.
	ReconnectDelay time.Duration

	// InboundQueue is the number of received frames that are buffered for the reader. When the
	// queue is full, new frames are dropped (see Dropped).
	InboundQueue int
}

// DefaultTunnelConfig works with KNXnet/IP interfaces and routers.
var DefaultTunnelConfig = TunnelConfig{
	Tunnel:         knxnet.DefaultTunnelConfig,
	ConfirmTimeout: 3 * time.Second,
	ReconnectDelay: 5 * time.Second,
	InboundQueue:   32,
}

func (cfg TunnelConfig) withDefaults() TunnelConfig {
	if cfg.ConfirmTimeout <= 0 {
		cfg.ConfirmTimeout = DefaultTunnelConfig.ConfirmTimeout
	}

	if cfg.ReconnectDelay <= 0 {
		cfg.ReconnectDelay = DefaultTunnelConfig.ReconnectDelay
	}

	if cfg.InboundQueue <= 0 {
		cfg.InboundQueue = DefaultTunnelConfig.InboundQueue
	}

	return cfg
}

// A Tunnel is a link through a KNXnet/IP tunnelling connection. Its individual address is assigned
// by the server. When the connection is lost, the tunnel connects again; Send fails with
// ErrDisconnected until it is back.
type Tunnel struct {
	config  TunnelConfig
	conn    *net.UDPConn
	control *net.UDPAddr

	requests chan *sendRequest
	closing  chan struct{}
	done     chan struct{}

	inbound chan *cemi.LDataInd
	dropped atomic.Uint32
	address atomic.Uint32

	sendMu    sync.Mutex
	closeOnce sync.Once
}

var (
	_ link.Link      = (*Tunnel)(nil)
	_ link.Addresser = (*Tunnel)(nil)
)

type sendRequest struct {
	msg    cemi.Message
	result chan error
}

type packet struct {
	data []byte
	from *net.UDPAddr
}

// DialTunnel connects to the KNXnet/IP server at address ("host" or "host:port").
func DialTunnel(address string, config TunnelConfig) (*Tunnel, error) {
	if _, _, err := net.SplitHostPort(address); err != nil {
		address = net.JoinHostPort(address, "3671")
	}

	control, err := net.ResolveUDPAddr("udp4", address)
	if err != nil {
		return nil, err
	}

	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return nil, err
	}

	config = config.withDefaults()

	t := &Tunnel{
		config:   config,
		conn:     conn,
		control:  control,
		requests: make(chan *sendRequest),
		closing:  make(chan struct{}),
		done:     make(chan struct{}),
		inbound:  make(chan *cemi.LDataInd, config.InboundQueue),
	}

	packets := make(chan packet, 16)
	go t.read(packets)

	connected := make(chan error, 1)
	go t.serve(packets, connected)

	if err := <-connected; err != nil {
		t.Close()
		return nil, err
	}

	return t, nil
}

// Address returns the individual address that the server assigned to the tunnel.
func (t *Tunnel) Address() cemi.IndividualAddr {
	return cemi.IndividualAddr(t.address.Load())
}

// Inbound returns the frames received from the bus.
func (t *Tunnel) Inbound() <-chan *cemi.LDataInd {
	return t.inbound
}

// Dropped returns the number of received frames that were dropped because the inbound queue was
// full.
func (t *Tunnel) Dropped() uint32 {
	return t.dropped.Load()
}

// Send transmits the frame and waits for its L_Data.con.
func (t *Tunnel) Send(req *cemi.LDataReq) error {
	t.sendMu.Lock()
	defer t.sendMu.Unlock()

	r := &sendRequest{msg: req, result: make(chan error, 1)}

	select {
	case t.requests <- r:
	case <-t.done:
		return link.ErrClosed
	}

	select {
	case err := <-r.result:
		return err
	case <-t.done:
		return link.ErrClosed
	}
}

// Close disconnects from the server.
func (t *Tunnel) Close() error {
	t.closeOnce.Do(func() { close(t.closing) })
	<-t.done

	return nil
}

// read passes packets from the socket to serve.
func (t *Tunnel) read(packets chan<- packet) {
	defer close(packets)

	buffer := make([]byte, 512)

	for {
		n, from, err := t.conn.ReadFromUDP(buffer)
		if err != nil {
			return
		}

		select {
		case packets <- packet{append([]byte(nil), buffer[:n]...), from}:
		case <-t.done:
			return
		}
	}
}

// serve runs the tunnel state machine.
func (t *Tunnel) serve(packets <-chan packet, connected chan<- error) {
	defer close(t.done)
	defer close(t.inbound)
	defer t.conn.Close()

	tun := knxnet.NewTunnel(t.config.Tunnel)
	tun.Connect(time.Now())

	var (
		pending       *sendRequest // waiting for the ack and the L_Data.con
		acked         bool
		confirmed     *cemi.LDataCon // L_Data.con that arrived before the ack
		confirmBefore time.Time      // deadline for the L_Data.con once acked
		reconnectAt   time.Time
		closing       = t.closing
		requests      = t.requests
		timer         = time.NewTimer(time.Hour)
	)

	timer.Stop()

	finish := func(err error) {
		if pending != nil {
			pending.result <- err
			pending = nil
		}
	}

	for {
		// Carry out the state machine's events.
		for {
			event, ok := tun.NextEvent()
			if !ok {
				break
			}

			switch event.Kind {
			case knxnet.EventSend:
				t.send(tun, event)

			case knxnet.EventConnected:
				t.address.Store(uint32(event.Address))

				if connected != nil {
					connected <- nil
					connected = nil
				}

			case knxnet.EventDisconnected:
				finish(ErrDisconnected)

				if connected != nil {
					connected <- event.Err
					return
				}

				if closing == nil {
					return
				}

				reconnectAt = time.Now().Add(t.config.ReconnectDelay)

			case knxnet.EventAcked:
				if event.Err != nil {
					finish(event.Err)
				} else if confirmed != nil {
					finish(confirmation(confirmed))
				} else {
					acked = true
					confirmBefore = time.Now().Add(t.config.ConfirmTimeout)
				}

			case knxnet.EventMessage:
				switch msg := event.Message.(type) {
				case *cemi.LDataInd:
					select {
					case t.inbound <- msg:
					default:
						t.dropped.Add(1)
					}

				case *cemi.LDataCon:
					if pending != nil && acked {
						finish(confirmation(msg))
					} else if pending != nil {
						confirmed = msg
					}
				}
			}
		}

		// Work out the next timeout.
		deadline, ok := tun.Deadline()

		if pending != nil && acked && (!ok || confirmBefore.Before(deadline)) {
			deadline, ok = confirmBefore, true
		}

		if tun.State() == knxnet.TunnelClosed && !reconnectAt.IsZero() {
			deadline, ok = reconnectAt, true
		}

		var tick <-chan time.Time

		if ok {
			timer.Reset(time.Until(deadline))
			tick = timer.C
		}

		// Accept a new frame only when the previous one is done.
		var accept chan *sendRequest
		if pending == nil {
			accept = requests
		}

		select {
		case p, open := <-packets:
			if !open {
				finish(link.ErrClosed)
				return
			}

			if p.from.IP.Equal(t.control.IP) {
				tun.HandlePacket(time.Now(), p.data)
			}

		case r := <-accept:
			if err := tun.Send(time.Now(), r.msg); err != nil {
				if err == knxnet.ErrNotConnected {
					err = ErrDisconnected
				}

				r.result <- err
				break
			}

			pending, acked, confirmed = r, false, nil

		case now := <-tick:
			switch {
			case pending != nil && acked && !now.Before(confirmBefore):
				finish(link.ErrConfirmTimeout)

			case tun.State() == knxnet.TunnelClosed && !reconnectAt.IsZero():
				reconnectAt = time.Time{}
				tun.Connect(now)

			default:
				tun.Tick(now)
			}

		case <-closing:
			closing = nil
			requests = nil

			if tun.State() == knxnet.TunnelClosed {
				return
			}

			tun.Disconnect(time.Now())
		}

		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
}

// send writes a packet to the control or data endpoint of the server.
func (t *Tunnel) send(tun *knxnet.Tunnel, event knxnet.Event) {
	to := t.control

	if data := tun.DataEndpoint(); event.ToData && !data.IsRouteBack() {
		to = &net.UDPAddr{IP: net.IP(data.IP[:]), Port: int(data.Port)}

		if to.IP.IsUnspecified() {
			to.IP = t.control.IP
		}
	}

	t.conn.WriteToUDP(event.Packet, to)
}

func confirmation(con *cemi.LDataCon) error {
	if con.Control1&cemi.Control1HasError != 0 {
		return link.ErrNotConfirmed
	}

	return nil
}
