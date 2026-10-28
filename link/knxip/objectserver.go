package knxip

import (
	"net"
	"sync"
	"time"

	"github.com/shabunin/tiny-knx-go/knxnet"
	"github.com/shabunin/tiny-knx-go/link"
)

// An ObjectServerClient is a connection to the ObjectServer of an IP BAOS over KNXnet/IP (UDP,
// connection type 0xF0). It is a baos.Transport. When the connection is lost, Inbound closes.
type ObjectServerClient struct {
	conn    *net.UDPConn
	control *net.UDPAddr

	requests chan *objectRequest
	closing  chan struct{}
	done     chan struct{}
	inbound  chan []byte

	sendMu    sync.Mutex
	closeOnce sync.Once
}

type objectRequest struct {
	message []byte
	result  chan error
}

// DialObjectServer connects to the ObjectServer of the KNXnet/IP server at address ("host" or
// "host:port"). The connection type of config is set to ObjectServer.
func DialObjectServer(address string, config knxnet.TunnelConfig) (*ObjectServerClient, error) {
	conn, control, err := listenFor(address)
	if err != nil {
		return nil, err
	}

	config.Type = knxnet.ObjectServerConnection

	c := &ObjectServerClient{
		conn:     conn,
		control:  control,
		requests: make(chan *objectRequest),
		closing:  make(chan struct{}),
		done:     make(chan struct{}),
		inbound:  make(chan []byte, 32),
	}

	packets := make(chan packet, 16)
	go readPackets(conn, packets, c.done)

	connected := make(chan error, 1)
	go c.serve(knxnet.NewTunnel(config), packets, connected)

	if err := <-connected; err != nil {
		c.Close()
		return nil, err
	}

	return c, nil
}

// Send sends a message of the ObjectServer protocol and waits until the server acknowledged it.
func (c *ObjectServerClient) Send(message []byte) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()

	r := &objectRequest{message: message, result: make(chan error, 1)}

	select {
	case c.requests <- r:
	case <-c.done:
		return link.ErrClosed
	}

	select {
	case err := <-r.result:
		return err
	case <-c.done:
		return link.ErrClosed
	}
}

// Inbound returns the messages from the ObjectServer: responses and indications.
func (c *ObjectServerClient) Inbound() <-chan []byte {
	return c.inbound
}

// Close disconnects from the server.
func (c *ObjectServerClient) Close() error {
	c.closeOnce.Do(func() { close(c.closing) })
	<-c.done

	return nil
}

// serve runs the connection state machine.
func (c *ObjectServerClient) serve(tun *knxnet.Tunnel, packets <-chan packet, connected chan<- error) {
	defer close(c.done)
	defer close(c.inbound)
	defer c.conn.Close()

	tun.Connect(time.Now())

	var (
		pending  *objectRequest // waiting for the ack
		closing  = c.closing
		requests = c.requests
		timer    = time.NewTimer(time.Hour)
	)

	timer.Stop()

	finish := func(err error) {
		if pending != nil {
			pending.result <- err
			pending = nil
		}
	}

	for {
		for {
			event, ok := tun.NextEvent()
			if !ok {
				break
			}

			switch event.Kind {
			case knxnet.EventSend:
				sendPacket(c.conn, c.control, tun, event)

			case knxnet.EventConnected:
				connected <- nil
				connected = nil

			case knxnet.EventDisconnected:
				finish(ErrDisconnected)

				if connected != nil {
					connected <- event.Err
				}

				return

			case knxnet.EventAcked:
				finish(event.Err)

			case knxnet.EventMessage:
				select {
				case c.inbound <- event.Data:
				default: // the reader is stuck; the client times out its request
				}
			}
		}

		var tick <-chan time.Time

		if deadline, ok := tun.Deadline(); ok {
			timer.Reset(time.Until(deadline))
			tick = timer.C
		}

		var accept chan *objectRequest
		if pending == nil {
			accept = requests
		}

		select {
		case p, open := <-packets:
			if !open {
				finish(link.ErrClosed)
				return
			}

			if p.from.IP.Equal(c.control.IP) {
				tun.HandlePacket(time.Now(), p.data)
			}

		case r := <-accept:
			if err := tun.SendObjectServer(time.Now(), r.message); err != nil {
				r.result <- err
				break
			}

			pending = r

		case now := <-tick:
			tun.Tick(now)

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
