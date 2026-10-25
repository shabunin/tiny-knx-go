package gateway

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"

	"github.com/shabunin/tiny-knx-go/baos"
	"github.com/shabunin/tiny-knx-go/link/ft12"
	"github.com/shabunin/tiny-knx-go/serial"
)

// BAOSPort is the default TCP port of the ObjectServer.
const BAOSPort = 12004

// Over TCP, an ObjectServer message follows a KNXnet/IP header with the service ObjectServer
// request (0xF080), which both sides send, and a connection header of 4 bytes that is all zeroes
// but its length:
//
//	06 20 F0 80 LL LL  04 00 00 00  F0 01 ...
const (
	baosHeaderSize  = 6 + 4
	baosServiceType = 0xF080
)

var errBAOSFrame = errors.New("not an ObjectServer frame")

func appendBAOSFrame(b, message []byte) []byte {
	b = append(b, 0x06, 0x20, baosServiceType>>8, baosServiceType&0xFF)
	b = binary.BigEndian.AppendUint16(b, uint16(baosHeaderSize+len(message)))

	return append(append(b, 0x04, 0, 0, 0), message...)
}

func readBAOSFrame(r *bufio.Reader) ([]byte, error) {
	var header [6]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}

	size := int(binary.BigEndian.Uint16(header[4:]))
	if header[0] != 0x06 || header[1] != 0x20 || binary.BigEndian.Uint16(header[2:]) != baosServiceType || size < baosHeaderSize {
		return nil, errBAOSFrame
	}

	body := make([]byte, size-6)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}

	if int(body[0]) > len(body) || body[0] < 4 {
		return nil, errBAOSFrame
	}

	return body[body[0]:], nil
}

// BAOSMedia describes the media that OpenBAOS takes.
const BAOSMedia = `  baos:PORT[?baud=19200]            BAOS module in ObjectServer mode on a serial port
  baos:tcp://HOST[:12004]           ObjectServer over TCP (IP BAOS, knxgw -o baos)`

// OpenBAOS connects to the ObjectServer of a spec of BAOSMedia. Opening a serial module switches it
// to ObjectServer mode; it is closed on SIGINT and SIGTERM.
func OpenBAOS(spec *Spec) (*baos.Client, error) {
	var (
		t      baos.Transport
		closer io.Closer
	)

	if host, ok := strings.CutPrefix(spec.Target, "tcp://"); ok {
		if _, _, err := net.SplitHostPort(host); err != nil {
			host = net.JoinHostPort(host, fmt.Sprint(BAOSPort))
		}

		conn, err := net.Dial("tcp", host)
		if err != nil {
			return nil, err
		}

		tcp := newBAOSConn(conn)
		t, closer = tcp, tcp
	} else {
		baud, err := spec.Int("baud", 19200)
		if err != nil {
			return nil, err
		}

		conn, err := openBAOSPort(spec.Target, baud)
		if err != nil {
			return nil, err
		}

		t, closer = conn, conn
	}

	if err := spec.Unused(); err != nil {
		closer.Close()
		return nil, err
	}

	CloseOnSignal(closer)

	c, err := baos.NewClient(t, baos.DefaultConfig)
	if err != nil {
		closer.Close()
		return nil, fmt.Errorf("opening the ObjectServer on %s: %v", spec.Target, err)
	}

	return c, nil
}

// OpenBAOSPort opens the ObjectServer of a serial module, which it switches to ObjectServer mode.
// Closing the returned closer closes the port; the module stays in ObjectServer mode.
func OpenBAOSPort(port string, baud int) (*baos.Client, io.Closer, error) {
	conn, err := openBAOSPort(port, baud)
	if err != nil {
		return nil, nil, err
	}

	c, err := baos.NewClient(conn, baos.DefaultConfig)
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("opening the ObjectServer on %s: %v", port, err)
	}

	return c, conn, nil
}

func openBAOSPort(name string, baud int) (*ft12.Conn, error) {
	port, err := serial.Open(name, baud, serial.ParityEven)
	if err != nil {
		return nil, err
	}

	conn := ft12.NewConn(port, ft12.DefaultConnConfig)
	if err := conn.Reset(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("resetting the module on %s: %v", name, err)
	}

	return conn, nil
}

// baosConn is a client connection to an ObjectServer over TCP.
type baosConn struct {
	conn    net.Conn
	inbound chan []byte
}

func newBAOSConn(conn net.Conn) *baosConn {
	c := &baosConn{conn: conn, inbound: make(chan []byte, 16)}
	go c.read()

	return c
}

func (c *baosConn) read() {
	defer close(c.inbound)

	r := bufio.NewReader(c.conn)

	for {
		message, err := readBAOSFrame(r)
		if err != nil {
			return
		}

		c.inbound <- message
	}
}

func (c *baosConn) Send(message []byte) error {
	_, err := c.conn.Write(appendBAOSFrame(nil, message))
	return err
}

func (c *baosConn) Inbound() <-chan []byte { return c.inbound }

func (c *baosConn) Close() error { return c.conn.Close() }

// A BAOSHub shares an ObjectServer between several users: each gets all indications. The
// ObjectServer may change, e.g. while its module is in data link layer mode the hub has none.
type BAOSHub struct {
	mu     sync.Mutex
	client *baos.Client
	gone   chan struct{} // closed when client goes
	subs   map[chan baos.Indication]bool
}

// NewBAOSHub returns a hub without an ObjectServer.
func NewBAOSHub() *BAOSHub {
	return &BAOSHub{subs: map[chan baos.Indication]bool{}}
}

// Use makes c the ObjectServer of the hub until its transport closes, which closes the returned
// channel and the connections of the TCP clients that came meanwhile.
func (h *BAOSHub) Use(c *baos.Client) <-chan struct{} {
	gone := make(chan struct{})

	h.mu.Lock()
	h.client, h.gone = c, gone
	h.mu.Unlock()

	go func() {
		for ind := range c.Indications() {
			h.mu.Lock()
			for sub := range h.subs {
				select {
				case sub <- ind:
				default: // a slow user misses indications rather than stalling the others
				}
			}
			h.mu.Unlock()
		}

		h.release(c)
		close(gone)
	}()

	return gone
}

// release takes c from the hub at once, before its transport finishes closing.
func (h *BAOSHub) release(c *baos.Client) {
	h.mu.Lock()
	if h.client == c {
		h.client = nil
	}
	h.mu.Unlock()
}

// current returns the ObjectServer and the channel that closes when it goes, or nil.
func (h *BAOSHub) current() (*baos.Client, <-chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.client, h.gone
}

// Client returns the ObjectServer, or nil.
func (h *BAOSHub) Client() *baos.Client {
	c, _ := h.current()
	return c
}

// Subscribe returns a channel of the indications from now on, of this ObjectServer and the next
// ones, and a function that ends the subscription.
func (h *BAOSHub) Subscribe() (<-chan baos.Indication, func()) {
	sub := make(chan baos.Indication, 32)

	h.mu.Lock()
	h.subs[sub] = true
	h.mu.Unlock()

	return sub, func() {
		h.mu.Lock()
		delete(h.subs, sub)
		h.mu.Unlock()
	}
}

// ServeBAOS serves the ObjectServer of the hub over TCP or a unix socket, like an IP BAOS: the
// requests of all clients go to the server one at a time, and every client gets the indications.
// While the hub has no ObjectServer, clients are turned away.
func ServeBAOS(l net.Listener, h *BAOSHub) error {
	for {
		conn, err := l.Accept()
		if err != nil {
			return err
		}

		go serveBAOSConn(conn, h)
	}
}

func serveBAOSConn(conn net.Conn, h *BAOSHub) {
	defer conn.Close()

	peer := "on " + conn.LocalAddr().String() // a unix socket client has no address
	if addr, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
		peer = addr.String()
	}

	c, gone := h.current()
	if c == nil {
		log.Printf("ObjectServer client %v turned away: the module is in data link layer mode", peer)
		return
	}

	log.Printf("ObjectServer client %v connected", peer)

	var writeMu sync.Mutex

	write := func(message []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()

		_, err := conn.Write(appendBAOSFrame(nil, message))

		return err
	}

	indications, unsubscribe := h.Subscribe()
	defer unsubscribe()

	stop := make(chan struct{})
	defer close(stop)

	go func() {
		for {
			select {
			case ind := <-indications:
				if ind.Raw == nil {
					continue // a restart, which has no message
				}

				if write(ind.Raw) != nil {
					conn.Close()
					return
				}

			case <-gone:
				log.Printf("ObjectServer client %v: the ObjectServer went away", peer)
				conn.Close()

				return

			case <-stop:
				return
			}
		}
	}()

	r := bufio.NewReader(conn)

	for {
		req, err := readBAOSFrame(r)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				log.Printf("ObjectServer client %v: %v", peer, err)
			}

			break
		}

		res, err := c.Request(req)
		if err != nil {
			log.Printf("ObjectServer client %v: request % x: %v", peer, req, err)
			continue
		}

		if err := write(res); err != nil {
			break
		}
	}

	log.Printf("ObjectServer client %v disconnected", peer)
}

// ListenBAOS listens on the IPv4 address of the network interface (all interfaces if empty), or on
// a unix socket if target is a path, which it replaces.
func ListenBAOS(target string, port int) (net.Listener, error) {
	if strings.Contains(target, "/") {
		if info, err := os.Stat(target); err == nil && info.Mode()&os.ModeSocket != 0 {
			os.Remove(target)
		}

		return net.Listen("unix", target)
	}

	ifi, err := Interface(target)
	if err != nil {
		return nil, err
	}

	host := ""
	if ifi != nil {
		ip := interfaceIPv4(ifi)
		if ip == nil {
			return nil, fmt.Errorf("%s has no IPv4 address", target)
		}

		host = ip.String()
	}

	return net.Listen("tcp", net.JoinHostPort(host, fmt.Sprint(port)))
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
