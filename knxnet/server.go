package knxnet

import (
	"time"

	"github.com/shabunin/tiny-knx-go/cemi"
)

// ServerConfig configures a Server. Zero durations take the values of DefaultServerConfig.
type ServerConfig struct {
	// Device describes the server in search and description responses. The service families
	// core, device management and tunnelling are added if Families is empty.
	Device DeviceInfo

	// Control is the endpoint that search responses advertise for connections.
	Control HPAI

	// Tunnels are the individual addresses of the tunnels, one per connection.
	Tunnels []cemi.IndividualAddr

	// AckTimeout is how long to wait for a TUNNELLING_ACK before sending the request once more.
	// A connection whose client does not acknowledge the repetition either is closed.
	AckTimeout time.Duration

	// ConnectionTimeout closes connections without a CONNECTIONSTATE_REQUEST for this long.
	ConnectionTimeout time.Duration

	// QueueSize limits the messages waiting for a busy client; more are dropped.
	QueueSize int

	// FirstChannel is the channel ID of the first connection (0 means 1). A server that restarts
	// should start somewhere else, so that a client still holding a channel of the previous run
	// does not talk to a new connection.
	FirstChannel uint8
}

// DefaultServerConfig contains the timings from the KNXnet/IP specification.
var DefaultServerConfig = ServerConfig{
	AckTimeout:        time.Second,
	ConnectionTimeout: 120 * time.Second,
	QueueSize:         32,
}

// ServerEventKind tells what a ServerEvent means.
type ServerEventKind uint8

// These are the server event kinds.
const (
	// ServerSend asks the caller to send Packet to the client endpoint To.
	ServerSend ServerEventKind = iota

	// ServerOpened reports a new connection. Address is the tunnel address; it is zero for a
	// device management connection.
	ServerOpened

	// ServerClosed reports that a connection was closed.
	ServerClosed

	// ServerMessage delivers a cEMI message from a client: an *cemi.LDataReq from a tunnel, which
	// the caller sends to the bus and confirms with Confirm, or a management message (e.g.
	// *cemi.MPropReadReq) from a device management connection, which the caller answers with
	// Indicate.
	ServerMessage
)

// A ServerEvent is an output of a Server.
type ServerEvent struct {
	Kind    ServerEventKind
	Channel uint8
	Address cemi.IndividualAddr // tunnel address, for ServerOpened and ServerMessage
	Packet  []byte              // for ServerSend
	To      HPAI                // for ServerSend
	Message cemi.Message        // for ServerMessage
}

// A connection of a Server.
type serverConn struct {
	channel    uint8
	tunnel     bool
	address    cemi.IndividualAddr
	control    HPAI
	data       HPAI
	seqRecv    uint8
	seqSend    uint8
	lastActive time.Time

	queue    []cemi.Message // queue[0] is pending once sent
	pending  []byte
	sentAt   time.Time
	repeated bool
}

// A Server is the server side of KNXnet/IP: it answers searches and description requests and
// serves tunnelling and device management connections. Like Tunnel, it does no I/O: the caller
// passes packets with HandlePacket, calls Tick when Deadline passes and carries out the events
// from NextEvent.
//
// Endpoints are HPAIs. The caller passes the source of every packet; route-back HPAIs in requests
// (0.0.0.0:0, used by clients behind NAT) are replaced by it.
type Server struct {
	config ServerConfig
	conns  []*serverConn
	next   uint8 // last channel ID

	events []ServerEvent
	read   int
}

// NewServer returns a server without connections.
func NewServer(config ServerConfig) *Server {
	d := DefaultServerConfig

	if config.AckTimeout <= 0 {
		config.AckTimeout = d.AckTimeout
	}

	if config.ConnectionTimeout <= 0 {
		config.ConnectionTimeout = d.ConnectionTimeout
	}

	if config.QueueSize <= 0 {
		config.QueueSize = d.QueueSize
	}

	if len(config.Device.Families) == 0 {
		config.Device.Families = []ServiceFamily{
			{FamilyCore, 1}, {FamilyDeviceManagement, 1}, {FamilyTunnelling, 1},
		}
	}

	return &Server{config: config, next: config.FirstChannel - 1}
}

// SetDevice changes the description of the server, e.g. its programming mode status.
func (s *Server) SetDevice(info DeviceInfo) {
	if len(info.Families) == 0 {
		info.Families = s.config.Device.Families
	}

	s.config.Device = info
}

// Device returns the description of the server.
func (s *Server) Device() DeviceInfo {
	return s.config.Device
}

// NextEvent returns the next pending event.
func (s *Server) NextEvent() (ServerEvent, bool) {
	if s.read == len(s.events) {
		s.events = s.events[:0]
		s.read = 0

		return ServerEvent{}, false
	}

	event := s.events[s.read]
	s.events[s.read] = ServerEvent{}
	s.read++

	return event, true
}

// Deadline returns when Tick must be called next.
func (s *Server) Deadline() (time.Time, bool) {
	var earliest time.Time

	for _, c := range s.conns {
		deadline := c.lastActive.Add(s.config.ConnectionTimeout)
		if c.pending != nil {
			if t := c.sentAt.Add(s.config.AckTimeout); t.Before(deadline) {
				deadline = t
			}
		}

		if earliest.IsZero() || deadline.Before(earliest) {
			earliest = deadline
		}
	}

	return earliest, !earliest.IsZero()
}

// Tick handles timeouts.
func (s *Server) Tick(now time.Time) {
	for i := 0; i < len(s.conns); i++ {
		c := s.conns[i]

		switch {
		case !now.Before(c.lastActive.Add(s.config.ConnectionTimeout)):
			s.disconnect(c)
			i--

		case c.pending != nil && !now.Before(c.sentAt.Add(s.config.AckTimeout)):
			if c.repeated {
				s.disconnect(c)
				i--

				continue
			}

			c.repeated = true
			c.sentAt = now
			s.send(c.data, c.pending)
		}
	}
}

// Indicate sends a cEMI message to a client: an *cemi.LDataInd from the bus to a tunnel, or the
// answer to a management message. It reports false if there is no such connection or its queue
// is full.
func (s *Server) Indicate(now time.Time, channel uint8, msg cemi.Message) bool {
	c := s.conn(channel)
	if c == nil || len(c.queue) >= s.config.QueueSize {
		return false
	}

	c.queue = append(c.queue, msg)
	s.flush(now, c)

	return true
}

// Confirm sends the L_Data.con for a request from a tunnel, after the caller sent it to the bus.
func (s *Server) Confirm(now time.Time, channel uint8, req *cemi.LDataReq, ok bool) {
	con := &cemi.LDataCon{LData: req.LData}
	if !ok {
		con.Control1 |= cemi.Control1HasError
	}

	s.Indicate(now, channel, con)
}

// Connections returns the channels and tunnel addresses of the open connections.
func (s *Server) Connections() map[uint8]cemi.IndividualAddr {
	m := make(map[uint8]cemi.IndividualAddr, len(s.conns))
	for _, c := range s.conns {
		m[c.channel] = c.address
	}

	return m
}

// Disconnect closes a connection, telling the client.
func (s *Server) Disconnect(channel uint8) {
	if c := s.conn(channel); c != nil {
		s.disconnect(c)
	}
}

// HandlePacket processes a packet that came from the endpoint from.
func (s *Server) HandlePacket(now time.Time, from HPAI, packet []byte) {
	service, err := Decode(packet)
	if err != nil {
		return
	}

	route := func(h HPAI) HPAI {
		if h.IsRouteBack() {
			return from
		}

		return h
	}

	switch r := service.(type) {
	case *SearchRequest:
		s.reply(route(r.Discovery), &SearchResponse{Control: s.config.Control, Device: s.config.Device, Extended: r.Extended})

	case *DescriptionRequest:
		s.reply(route(r.Control), &DescriptionResponse{Device: s.config.Device})

	case *ConnectRequest:
		s.connect(now, route(r.Control), route(r.Data), r)

	case *ConnectionStateRequest:
		status := StatusConnectionID
		if c := s.conn(r.Channel); c != nil {
			status = StatusOK
			c.lastActive = now
		}

		s.reply(route(r.Control), &ConnectionStateResponse{Channel: r.Channel, Status: status})

	case *DisconnectRequest:
		status := StatusConnectionID
		if c := s.conn(r.Channel); c != nil {
			status = StatusOK
			s.remove(c)
		}

		s.reply(route(r.Control), &DisconnectResponse{Channel: r.Channel, Status: status})

	case *TunnellingRequest:
		s.request(now, from, r.Channel, r.Sequence, r.Message, true)

	case *DeviceConfigRequest:
		s.request(now, from, r.Channel, r.Sequence, r.Message, false)

	case *TunnellingAck:
		s.acked(now, r.Channel, r.Sequence, r.Status, true)

	case *DeviceConfigAck:
		s.acked(now, r.Channel, r.Sequence, r.Status, false)
	}
}

func (s *Server) connect(now time.Time, control, data HPAI, r *ConnectRequest) {
	fail := func(status Status) {
		s.reply(control, &ConnectResponse{Status: status})
	}

	c := &serverConn{control: control, data: data, lastActive: now}

	switch r.Type {
	case TunnelConnection:
		if r.Layer != TunnelLinkLayer {
			fail(StatusTunnellingLayer)
			return
		}

		addr, ok := s.freeTunnel()
		if !ok {
			fail(StatusNoMoreConnections)
			return
		}

		c.tunnel = true
		c.address = addr

	case DeviceManagementConnection:

	default:
		fail(StatusConnectionType)
		return
	}

	if len(s.conns) >= 255 {
		fail(StatusNoMoreConnections)
		return
	}

	for s.next++; s.next == 0 || s.conn(s.next) != nil; s.next++ {
	}

	c.channel = s.next
	s.conns = append(s.conns, c)

	s.reply(control, &ConnectResponse{
		Channel: c.channel,
		Data:    s.config.Control,
		Type:    r.Type,
		Address: c.address,
	})
	s.emit(ServerEvent{Kind: ServerOpened, Channel: c.channel, Address: c.address})
}

func (s *Server) freeTunnel() (cemi.IndividualAddr, bool) {
next:
	for _, addr := range s.config.Tunnels {
		for _, c := range s.conns {
			if c.tunnel && c.address == addr {
				continue next
			}
		}

		return addr, true
	}

	return 0, false
}

// request handles a TUNNELLING_REQUEST or DEVICE_CONFIGURATION_REQUEST.
func (s *Server) request(now time.Time, from HPAI, channel, seq uint8, msg cemi.Message, tunnel bool) {
	ack := func(c *serverConn, status Status) {
		var service Service = &TunnellingAck{Channel: channel, Sequence: seq, Status: status}
		if !tunnel {
			service = &DeviceConfigAck{Channel: channel, Sequence: seq, Status: status}
		}

		to := from
		if c != nil {
			to = c.data
		}

		s.reply(to, service)
	}

	c := s.conn(channel)
	if c == nil || c.tunnel != tunnel {
		ack(nil, StatusConnectionID)
		return
	}

	switch seq {
	case c.seqRecv:
	case c.seqRecv - 1:
		ack(c, StatusOK) // our acknowledgement got lost
		return
	default:
		return
	}

	c.seqRecv++
	c.lastActive = now
	ack(c, StatusOK)

	if msg == nil {
		return
	}

	if req, ok := msg.(*cemi.LDataReq); ok && req.Source == 0 {
		req.Source = c.address
	}

	s.emit(ServerEvent{Kind: ServerMessage, Channel: channel, Address: c.address, Message: msg})
}

func (s *Server) acked(now time.Time, channel, seq uint8, status Status, tunnel bool) {
	c := s.conn(channel)
	if c == nil || c.tunnel != tunnel || c.pending == nil || seq != c.seqSend {
		return
	}

	// A negative acknowledgement drops the message too: the client will not take it.
	c.pending = nil
	c.queue = c.queue[1:]
	c.seqSend++
	c.lastActive = now

	s.flush(now, c)
}

// flush sends the next queued message of a connection if none is pending.
func (s *Server) flush(now time.Time, c *serverConn) {
	if c.pending != nil || len(c.queue) == 0 {
		return
	}

	var service Service = &TunnellingRequest{Channel: c.channel, Sequence: c.seqSend, Message: c.queue[0]}
	if !c.tunnel {
		service = &DeviceConfigRequest{Channel: c.channel, Sequence: c.seqSend, Message: c.queue[0]}
	}

	c.pending = Encode(service)
	c.sentAt = now
	c.repeated = false
	s.send(c.data, c.pending)
}

func (s *Server) disconnect(c *serverConn) {
	s.reply(c.control, &DisconnectRequest{Channel: c.channel, Control: s.config.Control})
	s.remove(c)
}

func (s *Server) remove(c *serverConn) {
	for i, other := range s.conns {
		if other == c {
			s.conns = append(s.conns[:i], s.conns[i+1:]...)
			break
		}
	}

	s.emit(ServerEvent{Kind: ServerClosed, Channel: c.channel, Address: c.address})
}

func (s *Server) conn(channel uint8) *serverConn {
	for _, c := range s.conns {
		if c.channel == channel {
			return c
		}
	}

	return nil
}

func (s *Server) reply(to HPAI, service Service) {
	s.send(to, Encode(service))
}

func (s *Server) send(to HPAI, packet []byte) {
	s.emit(ServerEvent{Kind: ServerSend, Packet: packet, To: to})
}

func (s *Server) emit(event ServerEvent) {
	s.events = append(s.events, event)
}
