//go:build !tinygo

package knxip

import (
	"encoding/binary"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"sync"
	"time"

	"github.com/shabunin/tiny-knx-go/cemi"
	"github.com/shabunin/tiny-knx-go/knxnet"
	"github.com/shabunin/tiny-knx-go/link"
)

// TunnelServerConfig configures a TunnelServer.
type TunnelServerConfig struct {
	// Server holds the description of the server, the tunnel addresses and the timings. Its
	// Control endpoint is filled in from the socket if it is zero, its FirstChannel at random.
	Server knxnet.ServerConfig

	// Attach connects a tunnel to the bus, with the tunnel address. The server closes the link
	// when the connection closes. Frames that the link receives go to the client if they are
	// addressed to a group or to the tunnel.
	Attach func(addr cemi.IndividualAddr) link.Link

	// ObjectServer, if set, serves each ObjectServer connection of a client, in a goroutine of
	// its own, until the connection closes. Server.ObjectServer gives the version of the
	// ObjectServer protocol, which the server announces and accepts connections for.
	ObjectServer func(conn *ObjectServerConn)

	// Log, if set, is called for connections that open and close.
	Log func(format string, args ...any)

	// Trace, if set, is called for every packet that the server receives or sends.
	Trace func(received bool, peer knxnet.HPAI, packet []byte)
}

// A TunnelServer is a KNXnet/IP interface: it answers searches and serves tunnelling
// connections, which ETS and other tools use to reach a bus. Each tunnel is a participant of the
// bus with its own individual address.
//
// Device management connections answer M_PropRead requests for the properties that tools read
// from an interface (serial number, individual address, name and the like) from the server
// description.
//
// Like an IP BAOS, the server can also serve the ObjectServer protocol of a BAOS on connections of
// type 0xF0 (TunnelServerConfig.ObjectServer).
type TunnelServer struct {
	config  TunnelServerConfig
	control *net.UDPConn
	search  *net.UDPConn

	// Connections over TCP: the listener and the open streams by peer.
	listener  *net.TCPListener
	streamsMu sync.Mutex
	streams   map[knxnet.HPAI]net.Conn

	mu     sync.Mutex
	server *knxnet.Server

	packets chan serverPacket
	frames  chan tunnelFrame
	done    chan struct{}
	once    sync.Once
	wg      sync.WaitGroup

	tunnels map[uint8]*serverTunnel // by channel; touched only by serve

	objects map[uint8]*ObjectServerConn // by channel; touched only by serve
	outbox  chan objectMessage
}

// An ObjectServerConn is a connection of a client to the ObjectServer of a TunnelServer.
type ObjectServerConn struct {
	// Channel is the channel ID of the connection.
	Channel uint8

	server   *TunnelServer
	requests chan []byte
	done     chan struct{} // closed when the connection closes
}

// A message of the ObjectServer protocol to a client, or (close set) the end of its connection.
type objectMessage struct {
	conn  *ObjectServerConn
	data  []byte
	close bool
}

// Requests returns the messages of the ObjectServer protocol from the client. The channel closes
// when the connection closes.
func (c *ObjectServerConn) Requests() <-chan []byte { return c.requests }

// Send sends a message of the ObjectServer protocol to the client: the response to a request, or an
// indication. It reports false if the connection is closed.
func (c *ObjectServerConn) Send(message []byte) bool {
	return c.post(objectMessage{conn: c, data: message})
}

// Close closes the connection, telling the client.
func (c *ObjectServerConn) Close() {
	c.post(objectMessage{conn: c, close: true})
}

func (c *ObjectServerConn) post(m objectMessage) bool {
	select {
	case c.server.outbox <- m:
		return true
	case <-c.done:
	case <-c.server.done:
	}

	return false
}

type serverPacket struct {
	from   knxnet.HPAI
	data   []byte
	closed bool // the TCP stream from the peer closed
}

// A frame from the bus to a tunnel, or the result of sending one from it (req set).
type tunnelFrame struct {
	tunnel *serverTunnel
	ind    *cemi.LDataInd
	req    *cemi.LDataReq
	err    error
}

type serverTunnel struct {
	channel uint8
	address cemi.IndividualAddr
	link    link.Link
	send    chan *cemi.LDataReq

	// sending is held while a frame is sent and confirmed, so that answers to it (e.g. a T_ACK on
	// the in-memory bus, which arrives before Send returns) reach the client after the L_Data.con,
	// as on a real bus. Clients such as Calimero ignore them otherwise.
	sending sync.Mutex
}

// ListenTunnelServer serves on the given network interface: connections over UDP and TCP on port
// (3671 if zero) of its IPv4 address and searches on the routing multicast group. The control
// socket may share the port with routing participants on the same host.
func ListenTunnelServer(ifi *net.Interface, port int, config TunnelServerConfig) (*TunnelServer, error) {
	if port == 0 {
		port = knxnet.Port
	}

	var ip net.IP
	if ifi != nil {
		ip = interfaceIPv4(ifi)
	}

	// The multicast socket is bound to 0.0.0.0:3671 with SO_REUSEADDR, so the control socket
	// needs it too to share the port.
	search, err := net.ListenMulticastUDP("udp4", ifi, &net.UDPAddr{IP: net.IP(knxnet.RoutingGroup[:]), Port: knxnet.Port})
	if err != nil {
		return nil, err
	}

	control, err := listenReusable(&net.UDPAddr{IP: ip, Port: port})
	if err != nil {
		search.Close()
		return nil, err
	}

	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: ip, Port: control.LocalAddr().(*net.UDPAddr).Port})
	if err != nil {
		search.Close()
		control.Close()

		return nil, err
	}

	return newTunnelServer(control, search, listener, config), nil
}

// NewTunnelServer serves connections on control and, if it is not nil, searches arriving on
// search. It takes ownership of both sockets.
func NewTunnelServer(control, search *net.UDPConn, config TunnelServerConfig) *TunnelServer {
	return newTunnelServer(control, search, nil, config)
}

func newTunnelServer(control, search *net.UDPConn, listener *net.TCPListener, config TunnelServerConfig) *TunnelServer {
	config.Server.TCP = listener != nil

	if c := &config.Server.Control; c.Port == 0 {
		self := control.LocalAddr().(*net.UDPAddr)
		c.Protocol = knxnet.ProtocolUDP
		c.Port = uint16(self.Port)
		copy(c.IP[:], self.IP.To4())
	}

	if config.Server.FirstChannel == 0 {
		config.Server.FirstChannel = uint8(rand.Uint32())
	}

	s := &TunnelServer{
		config:   config,
		control:  control,
		search:   search,
		listener: listener,
		streams:  make(map[knxnet.HPAI]net.Conn),
		server:   knxnet.NewServer(config.Server),
		packets:  make(chan serverPacket, 16),
		frames:   make(chan tunnelFrame, 64),
		done:     make(chan struct{}),
		tunnels:  make(map[uint8]*serverTunnel),
		objects:  make(map[uint8]*ObjectServerConn),
		outbox:   make(chan objectMessage, 64),
	}

	s.wg.Add(1)

	go s.read(control)

	if search != nil {
		s.wg.Add(1)

		go s.read(search)
	}

	if listener != nil {
		s.wg.Add(1)

		go s.accept()
	}

	go s.serve()

	return s
}

// Control returns the endpoint for connections, as search responses advertise it.
func (s *TunnelServer) Control() knxnet.HPAI {
	return s.config.Server.Control
}

// SetDevice changes the description of the server in search and description responses.
func (s *TunnelServer) SetDevice(info knxnet.DeviceInfo) {
	s.mu.Lock()
	s.server.SetDevice(info)
	s.mu.Unlock()
}

// Close disconnects all clients and stops the server.
func (s *TunnelServer) Close() error {
	s.once.Do(func() {
		close(s.done)
		s.control.Close()

		if s.search != nil {
			s.search.Close()
		}

		s.closeStreams()
	})

	return nil
}

func (s *TunnelServer) logf(format string, args ...any) {
	if s.config.Log != nil {
		s.config.Log(format, args...)
	}
}

func (s *TunnelServer) trace(received bool, peer knxnet.HPAI, packet []byte) {
	if s.config.Trace != nil {
		s.config.Trace(received, peer, packet)
	}
}

// write sends the packet of a ServerSend event.
func (s *TunnelServer) write(event knxnet.ServerEvent) {
	s.trace(false, event.To, event.Packet)

	if event.To.Protocol == knxnet.ProtocolTCP {
		s.streamsMu.Lock()
		conn := s.streams[event.To]
		s.streamsMu.Unlock()

		if conn != nil {
			conn.SetWriteDeadline(time.Now().Add(time.Second))
			conn.Write(event.Packet)
		}

		return
	}

	s.control.WriteToUDP(event.Packet, &net.UDPAddr{IP: net.IP(event.To.IP[:]), Port: int(event.To.Port)})
}

// accept takes connections over TCP.
func (s *TunnelServer) accept() {
	defer s.wg.Done()

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if isClosed(err) {
				return
			}

			continue
		}

		var peer knxnet.HPAI
		addr := conn.RemoteAddr().(*net.TCPAddr)
		peer.Protocol = knxnet.ProtocolTCP
		peer.Port = uint16(addr.Port)
		copy(peer.IP[:], addr.IP.To4())

		s.streamsMu.Lock()
		s.streams[peer] = conn
		s.streamsMu.Unlock()

		s.logf("TCP connection from %v", conn.RemoteAddr())
		s.wg.Add(1)

		go s.readStream(conn, peer)
	}
}

// readStream reads the packets of a TCP stream, and reports when it closes.
func (s *TunnelServer) readStream(conn net.Conn, peer knxnet.HPAI) {
	defer s.wg.Done()

	for {
		header := make([]byte, 6)
		if _, err := io.ReadFull(conn, header); err != nil {
			break
		}

		total := int(binary.BigEndian.Uint16(header[4:]))
		if header[0] != 6 || total < 6 || total > 1024 {
			break
		}

		packet := make([]byte, total)
		copy(packet, header)

		if _, err := io.ReadFull(conn, packet[6:]); err != nil {
			break
		}

		select {
		case s.packets <- serverPacket{from: peer, data: packet}:
		case <-s.done:
			return
		}
	}

	conn.Close()

	s.streamsMu.Lock()
	delete(s.streams, peer)
	s.streamsMu.Unlock()

	select {
	case s.packets <- serverPacket{from: peer, closed: true}:
	case <-s.done:
	}
}

func (s *TunnelServer) closeStreams() {
	if s.listener != nil {
		s.listener.Close()
	}

	s.streamsMu.Lock()
	for _, conn := range s.streams {
		conn.Close()
	}
	s.streamsMu.Unlock()
}

func (s *TunnelServer) read(conn *net.UDPConn) {
	defer s.wg.Done()

	buffer := make([]byte, 1024)

	for {
		n, from, err := conn.ReadFromUDP(buffer)
		if err != nil {
			if isClosed(err) {
				return
			}

			continue
		}

		p := serverPacket{data: append([]byte(nil), buffer[:n]...)}
		p.from.Protocol = knxnet.ProtocolUDP
		p.from.Port = uint16(from.Port)
		copy(p.from.IP[:], from.IP.To4())

		select {
		case s.packets <- p:
		case <-s.done:
			return
		}
	}
}

func (s *TunnelServer) serve() {
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()

	for {
		s.mu.Lock()
		deadline, ok := s.server.Deadline()
		s.mu.Unlock()

		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}

		var tick <-chan time.Time

		if ok {
			timer.Reset(time.Until(deadline))
			tick = timer.C
		}

		select {
		case <-s.done:
			s.shutdown()
			return

		case p := <-s.packets:
			if p.closed {
				s.logf("TCP connection from %s:%d closed", net.IP(p.from.IP[:]), p.from.Port)
				s.handle(func(time.Time) { s.server.StreamClosed(p.from) })

				continue
			}

			s.trace(true, p.from, p.data)
			s.handle(func(now time.Time) { s.server.HandlePacket(now, p.from, p.data) })

		case f := <-s.frames:
			if s.tunnels[f.tunnel.channel] != f.tunnel {
				continue // closed meanwhile
			}

			if f.req != nil {
				s.handle(func(now time.Time) { s.server.Confirm(now, f.tunnel.channel, f.req, f.err == nil) })
			} else {
				s.handle(func(now time.Time) { s.server.Indicate(now, f.tunnel.channel, f.ind) })
			}

		case m := <-s.outbox:
			if s.objects[m.conn.Channel] != m.conn {
				continue // closed meanwhile
			}

			if m.close {
				s.handle(func(time.Time) { s.server.Disconnect(m.conn.Channel) })
				continue
			}

			s.handle(func(now time.Time) {
				if !s.server.SendObjectServer(now, m.conn.Channel, m.data) {
					s.logf("ObjectServer connection %d: dropped a message for the busy client", m.conn.Channel)
				}
			})

		case now := <-tick:
			s.handle(func(time.Time) { s.server.Tick(now) })
		}
	}
}

// handle runs f on the server and carries out the events.
func (s *TunnelServer) handle(f func(now time.Time)) {
	s.mu.Lock()
	defer s.mu.Unlock()

	f(time.Now())

	for {
		event, ok := s.server.NextEvent()
		if !ok {
			return
		}

		switch event.Kind {
		case knxnet.ServerSend:
			s.write(event)

		case knxnet.ServerOpened:
			secured := ""
			if event.Session != 0 {
				secured = fmt.Sprintf(" in secure session %d of user %d", event.Session, event.User)
			}

			switch event.Type {
			case knxnet.DeviceManagementConnection:
				s.logf("Management connection %d opened%s", event.Channel, secured)
				continue

			case knxnet.ObjectServerConnection:
				s.logf("ObjectServer connection %d opened%s", event.Channel, secured)
				s.openObjectServer(event.Channel)

				continue
			}

			s.logf("Tunnel %d opened with address %v%s", event.Channel, event.Address, secured)
			s.open(event.Channel, event.Address)

		case knxnet.ServerSecure:
			s.logf("Secure session %d of user %d: %s", event.Session, event.User, event.Code.String())

		case knxnet.ServerClosed:
			s.logf("Connection %d closed", event.Channel)

			if t, ok := s.tunnels[event.Channel]; ok {
				delete(s.tunnels, event.Channel)
				close(t.send)
				t.link.Close()
			}

			s.closeObjectServer(event.Channel)

		case knxnet.ServerMessage:
			if event.Type == knxnet.ObjectServerConnection {
				if c, ok := s.objects[event.Channel]; ok {
					select {
					case c.requests <- event.Data:
					default:
						s.logf("ObjectServer connection %d: dropped a request, the ObjectServer is busy", event.Channel)
					}
				}

				continue
			}

			// Management messages may come on tunnels too, e.g. ETS reads the maximum APDU length.
			if req, ok := event.Message.(*cemi.LDataReq); ok {
				if t, ok := s.tunnels[event.Channel]; ok {
					select {
					case t.send <- req:
					default:
						s.server.Confirm(time.Now(), event.Channel, req, false)
					}
				}
			} else if answer := s.management(event.Message); answer != nil {
				s.server.Indicate(time.Now(), event.Channel, answer)
			}
		}
	}
}

// open attaches a new tunnel to the bus.
func (s *TunnelServer) open(channel uint8, addr cemi.IndividualAddr) {
	t := &serverTunnel{channel: channel, address: addr, link: s.config.Attach(addr), send: make(chan *cemi.LDataReq, 8)}
	s.tunnels[channel] = t

	deliver := func(f tunnelFrame) bool {
		select {
		case s.frames <- f:
			return true
		case <-s.done:
			return false
		}
	}

	go func() {
		for req := range t.send {
			t.sending.Lock()
			err := t.link.Send(req)
			ok := deliver(tunnelFrame{tunnel: t, req: req, err: err})
			t.sending.Unlock()

			if !ok {
				return
			}
		}
	}()

	go func() {
		for ind := range t.link.Inbound() {
			if ind.Control2.IsGroupAddr() || cemi.IndividualAddr(ind.Destination) == addr {
				t.sending.Lock()
				ok := deliver(tunnelFrame{tunnel: t, ind: ind})
				t.sending.Unlock()

				if !ok {
					return
				}
			}
		}
	}()
}

// openObjectServer serves a new ObjectServer connection.
func (s *TunnelServer) openObjectServer(channel uint8) {
	if s.config.ObjectServer == nil {
		s.server.Disconnect(channel)
		return
	}

	c := &ObjectServerConn{Channel: channel, server: s, requests: make(chan []byte, 16), done: make(chan struct{})}
	s.objects[channel] = c

	go s.config.ObjectServer(c)
}

func (s *TunnelServer) closeObjectServer(channel uint8) {
	if c, ok := s.objects[channel]; ok {
		delete(s.objects, channel)
		close(c.requests)
		close(c.done)
	}
}

func (s *TunnelServer) shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for channel := range s.server.Connections() {
		s.server.Disconnect(channel)
	}

	for {
		event, ok := s.server.NextEvent()
		if !ok {
			break
		}

		if event.Kind == knxnet.ServerSend {
			s.write(event)
		}
	}

	for channel, t := range s.tunnels {
		delete(s.tunnels, channel)
		close(t.send)
		t.link.Close()
	}

	for channel := range s.objects {
		s.closeObjectServer(channel)
	}

	s.control.Close()
	s.closeStreams()
	s.wg.Wait()
}

// Interface object types and properties of an interface that tools read on a device management
// connection.
const (
	objectTypeIPParameter = 11

	pidSerialNumber   = 11
	pidManufacturerID = 12
	pidMaxAPDULength  = 56
	pidDescriptor     = 83
	pidMediumType     = 51 // cEMI server object
	pidKNXAddress     = 52 // KNXnet/IP parameter object
	pidAdditionalIA   = 53
	pidCurrentIP      = 57
	pidMAC            = 64
	pidFriendlyName   = 76

	errVoidProperty = 0x07
	errReadOnly     = 0x05
)

// management answers a message from a device management connection.
func (s *TunnelServer) management(msg cemi.Message) cemi.Message {
	switch m := msg.(type) {
	case *cemi.MPropReadReq:
		con := &cemi.MPropReadCon{MProp: m.MProp}

		value, size := s.property(m.ObjectType, m.PropertyID)
		if value == nil {
			con.Count = 0
			con.Data = []byte{errVoidProperty}

			return con
		}

		if m.Start == 0 {
			con.Count = 1
			con.Data = []byte{0, byte(len(value) / size)}

			return con
		}

		first := int(m.Start-1) * size
		last := first + int(m.Count)*size

		if m.Count == 0 || last > len(value) {
			con.Count = 0
			con.Data = []byte{errVoidProperty}

			return con
		}

		con.Data = value[first:last]

		return con

	case *cemi.MPropWriteReq:
		con := &cemi.MPropWriteCon{MProp: m.MProp}
		con.Count = 0
		con.Data = []byte{errReadOnly}

		return con
	}

	return nil
}

// property returns the value of an interface property and the size of an element.
func (s *TunnelServer) property(objectType uint16, id uint8) ([]byte, int) {
	info := s.server.Device()

	switch objectType {
	case cemi.ObjectTypeDevice:
		switch id {
		case 1:
			return []byte{0, 0}, 2
		case pidSerialNumber:
			return info.SerialNumber[:], 6
		case pidManufacturerID:
			return info.SerialNumber[:2], 2
		case pidMaxAPDULength:
			return []byte{0, knxnet.MaxAPDU}, 2
		case pidDescriptor:
			return []byte{knxnet.RouterMask >> 8, knxnet.RouterMask & 0xff}, 2
		}

	case cemi.ObjectTypeCEMIServer:
		switch id {
		case 1:
			return []byte{0, byte(cemi.ObjectTypeCEMIServer)}, 2
		case pidMediumType:
			return []byte{0, 0x02}, 2 // TP1
		case cemi.PIDCommMode:
			return []byte{cemi.CommModeDataLinkLayer}, 1
		}

	case objectTypeIPParameter:
		switch id {
		case 1:
			return []byte{0, objectTypeIPParameter}, 2
		case pidKNXAddress:
			return []byte{byte(info.Address >> 8), byte(info.Address)}, 2
		case pidAdditionalIA:
			var b []byte
			for _, addr := range s.config.Server.Tunnels {
				b = append(b, byte(addr>>8), byte(addr))
			}

			return b, 2
		case pidCurrentIP:
			return s.config.Server.Control.IP[:], 4
		case pidMAC:
			return info.MAC[:], 6
		case pidFriendlyName:
			var name [30]byte
			copy(name[:], info.Name)

			return name[:], 1
		}
	}

	return nil, 0
}
