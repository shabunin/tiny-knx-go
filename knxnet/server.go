package knxnet

import (
	"crypto/aes"
	"crypto/cipher"
	"time"

	"github.com/shabunin/tiny-knx-go/cemi"
)

// ServerConfig configures a Server. Zero durations take the values of DefaultServerConfig.
type ServerConfig struct {
	// Device describes the server in search and description responses. The service families
	// core, device management and tunnelling are added if Families is empty, and security if
	// Secure is set.
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

	// Secure, if set, makes tunnelling and device management need a secure session.
	Secure *SecureConfig

	// TCP tells that the caller also takes connections over TCP, which the server then
	// announces as KNXnet/IP core v2. ETS opens secure sessions only over TCP.
	TCP bool

	// ObjectServer, if not zero, is the version of the ObjectServer protocol of a BAOS (0x20 for
	// 2.0) that the server serves on connections of type ObjectServerConnection. Search responses
	// then carry its service family and ObjectServerData, as those of an IP BAOS.
	ObjectServer uint8
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

	// ServerOpened reports a new connection of Type. Address is the tunnel address; it is zero
	// for other connections.
	ServerOpened

	// ServerClosed reports that a connection was closed.
	ServerClosed

	// ServerMessage delivers a cEMI message from a client: an *cemi.LDataReq from a tunnel, which
	// the caller sends to the bus and confirms with Confirm, or a management message (e.g.
	// *cemi.MPropReadReq) from a device management connection, which the caller answers with
	// Indicate. On an ObjectServer connection, Data is a message of the ObjectServer protocol,
	// which the caller answers with SendObjectServer.
	ServerMessage

	// ServerSecure reports the authentication of a user in a secure session, or the end of the
	// session: Code is SessionAuthenticated, SessionAuthFailed, SessionClose or SessionTimeout.
	ServerSecure
)

// A ServerEvent is an output of a Server.
type ServerEvent struct {
	Kind    ServerEventKind
	Channel uint8
	Type    ConnectionType      // for ServerOpened, ServerClosed and ServerMessage
	Address cemi.IndividualAddr // tunnel address, for ServerOpened and ServerMessage
	Packet  []byte              // for ServerSend
	To      HPAI                // for ServerSend
	Message cemi.Message        // for ServerMessage
	Data    []byte              // for ServerMessage on an ObjectServer connection

	// Session and User identify the secure session of a connection or a ServerSecure event, User
	// is zero for plain connections.
	Session uint16
	User    uint8
	Code    SessionCode // for ServerSecure
}

// A connection of a Server.
type serverConn struct {
	channel    uint8
	kind       ConnectionType
	address    cemi.IndividualAddr
	control    HPAI
	data       HPAI
	seqRecv    uint8
	seqSend    uint8
	lastActive time.Time

	queue    []queued // queue[0] is pending once sent
	pending  []byte
	sentAt   time.Time
	repeated bool

	session *session // nil for plain connections

	// stream is true for connections over TCP, whose requests are not acknowledged: the stream
	// delivers them reliably and in order.
	stream bool
}

// A message waiting to be sent on a connection: a cEMI message, or one of the ObjectServer
// protocol.
type queued struct {
	msg  cemi.Message
	data []byte
}

// A Server is the server side of KNXnet/IP: it answers searches and description requests and
// serves tunnelling and device management connections, and those of the ObjectServer protocol if
// configured. Like Tunnel, it does no I/O: the caller
// passes packets with HandlePacket, calls Tick when Deadline passes and carries out the events
// from NextEvent.
//
// Endpoints are HPAIs. The caller passes the source of every packet; route-back HPAIs in requests
// (0.0.0.0:0, used by clients behind NAT and over TCP) are replaced by it. For TCP, the source is
// an HPAI with ProtocolTCP and the address of the peer of the stream, and packets for it go back
// on that stream; the caller reports closed streams with StreamClosed.
type Server struct {
	config ServerConfig
	conns  []*serverConn
	next   uint8 // last channel ID

	secure      *SecureConfig
	deviceAuth  cipher.Block
	sessions    []*session
	lastSession uint16
	current     *session // the session of the packet being handled

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
		core := uint8(1)
		if config.TCP {
			core = 2
		}

		config.Device.Families = []ServiceFamily{{FamilyCore, core}, {FamilyDeviceManagement, 1}}

		// Without tunnels, a BAOS has three families, so that its manufacturer data comes at
		// the offset where the ObjectServer protocol shows it.
		if len(config.Tunnels) > 0 || config.ObjectServer == 0 {
			config.Device.Families = append(config.Device.Families, ServiceFamily{FamilyTunnelling, 1})
		}
	}

	if v := config.ObjectServer; v != 0 {
		d := &config.Device
		if !d.Supports(FamilyObjectServer) {
			d.Families = append(d.Families[:len(d.Families):len(d.Families)], ServiceFamily{FamilyObjectServer, v >> 4})
		}

		if len(d.ManufacturerData) == 0 {
			d.ManufacturerData = ObjectServerData(v)
		}
	}

	s := &Server{config: config, next: config.FirstChannel - 1}

	if config.Secure != nil {
		s.secure = config.Secure.withDefaults()
		s.deviceAuth, _ = aes.NewCipher(s.secure.DeviceAuth[:])

		d := &s.config.Device
		if !d.Supports(FamilySecurity) {
			d.Families = append(d.Families[:len(d.Families):len(d.Families)], ServiceFamily{FamilySecurity, 1})
		}

		if len(d.Secured) == 0 {
			d.Secured = []ServiceFamily{{FamilyDeviceManagement, 1}, {FamilyTunnelling, 1}}
		}
	}

	return s
}

// SetDevice changes the description of the server, e.g. its programming mode status.
func (s *Server) SetDevice(info DeviceInfo) {
	if len(info.Families) == 0 {
		info.Families = s.config.Device.Families
	}

	if len(info.Secured) == 0 {
		info.Secured = s.config.Device.Secured
	}

	if len(info.ManufacturerData) == 0 {
		info.ManufacturerData = s.config.Device.ManufacturerData
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

	for _, sess := range s.sessions {
		if deadline := sess.lastActive.Add(s.secure.SessionTimeout); earliest.IsZero() || deadline.Before(earliest) {
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
			s.sendVia(c.session, c.data, c.pending)
		}
	}

	for i := 0; i < len(s.sessions); i++ {
		if sess := s.sessions[i]; !now.Before(sess.lastActive.Add(s.secure.SessionTimeout)) {
			s.sendVia(sess, sess.control, Encode(&SessionStatus{Code: SessionTimeout}))
			s.closeSession(sess, SessionTimeout)
			i--
		}
	}
}

// Indicate sends a cEMI message to a client: an *cemi.LDataInd from the bus to a tunnel, or the
// answer to a management message. It reports false if there is no such connection or its queue
// is full.
func (s *Server) Indicate(now time.Time, channel uint8, msg cemi.Message) bool {
	c := s.conn(channel)
	if c == nil || c.kind == ObjectServerConnection || len(c.queue) >= s.config.QueueSize {
		return false
	}

	c.queue = append(c.queue, queued{msg: msg})
	s.flush(now, c)

	return true
}

// SendObjectServer sends a message of the ObjectServer protocol to the client of an ObjectServer
// connection: the response to a request, or an indication. It reports false if there is no such
// connection or its queue is full.
func (s *Server) SendObjectServer(now time.Time, channel uint8, message []byte) bool {
	c := s.conn(channel)
	if c == nil || c.kind != ObjectServerConnection || len(c.queue) >= s.config.QueueSize {
		return false
	}

	c.queue = append(c.queue, queued{data: message})
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

// StreamClosed closes the connections and secure sessions on a TCP stream that closed.
func (s *Server) StreamClosed(peer HPAI) {
	for i := 0; i < len(s.sessions); i++ {
		if sess := s.sessions[i]; sess.control == peer {
			s.closeSession(sess, SessionClose)
			i--
		}
	}

	for i := 0; i < len(s.conns); i++ {
		if c := s.conns[i]; c.stream && c.control == peer {
			s.remove(c)
			i--
		}
	}
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

	if s.secure != nil {
		switch r := service.(type) {
		case *SessionRequest:
			s.sessionRequest(now, routeBack(r.Control, from), r)
			return

		case *SecureWrapper:
			s.wrapped(now, from, r)
			return

		case *ConnectRequest:
			s.reply(routeBack(r.Control, from), &ConnectResponse{Status: StatusConnectionType})
			return
		}
	}

	s.handle(now, from, service)
}

func routeBack(h, from HPAI) HPAI {
	if h.IsRouteBack() {
		return from
	}

	return h
}

// handle processes a plain packet, or the packet of a secure session in s.current.
func (s *Server) handle(now time.Time, from HPAI, service Service) {
	route := func(h HPAI) HPAI { return routeBack(h, from) }

	switch r := service.(type) {
	case *SearchRequest:
		if device, ok := s.searchResponse(r); ok {
			s.reply(route(r.Discovery), &SearchResponse{Control: s.config.Control, Device: device, Extended: r.Extended})
		}

	case *DescriptionRequest:
		s.reply(route(r.Control), &DescriptionResponse{Device: s.config.Device})

	case *ConnectRequest:
		s.connect(now, route(r.Control), route(r.Data), r)

	case *ConnectionStateRequest:
		status := StatusConnectionID
		if c := s.peer(r.Channel); c != nil {
			status = StatusOK
			c.lastActive = now
		}

		s.reply(route(r.Control), &ConnectionStateResponse{Channel: r.Channel, Status: status})

	case *DisconnectRequest:
		status := StatusConnectionID
		if c := s.peer(r.Channel); c != nil {
			status = StatusOK
			s.remove(c)
		}

		s.reply(route(r.Control), &DisconnectResponse{Channel: r.Channel, Status: status})

	case *TunnellingRequest:
		s.request(now, from, TunnelConnection, r.Channel, r.Sequence, queued{msg: r.Message})

	case *DeviceConfigRequest:
		s.request(now, from, DeviceManagementConnection, r.Channel, r.Sequence, queued{msg: r.Message})

	case *ObjectServerRequest:
		s.request(now, from, ObjectServerConnection, r.Channel, r.Sequence, queued{data: r.Message})

	case *TunnellingAck:
		s.acked(now, TunnelConnection, r.Channel, r.Sequence)

	case *DeviceConfigAck:
		s.acked(now, DeviceManagementConnection, r.Channel, r.Sequence)

	case *ObjectServerAck:
		s.acked(now, ObjectServerConnection, r.Channel, r.Sequence)
	}
}

func (s *Server) connect(now time.Time, control, data HPAI, r *ConnectRequest) {
	fail := func(status Status) {
		s.reply(control, &ConnectResponse{Status: status})
	}

	c := &serverConn{kind: r.Type, control: control, data: data, lastActive: now, session: s.current, stream: control.Protocol == ProtocolTCP}

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

		c.address = addr

	case DeviceManagementConnection:
		if c.session != nil && c.session.user != 1 {
			fail(StatusConnectionType)
			return
		}

	case ObjectServerConnection:
		if s.config.ObjectServer == 0 || c.session != nil && c.session.user != 1 {
			fail(StatusConnectionType)
			return
		}

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

	dataEndpoint := s.config.Control
	if c.stream {
		dataEndpoint = HPAI{Protocol: ProtocolTCP}
	}

	s.reply(control, &ConnectResponse{
		Channel: c.channel,
		Data:    dataEndpoint,
		Type:    r.Type,
		Address: c.address,
	})
	s.emit(ServerEvent{Kind: ServerOpened, Channel: c.channel, Type: c.kind, Address: c.address, Session: c.sessionID(), User: c.user()})
}

// searchResponse describes the server for a search request, with the DIBs that it asks for, or
// returns false if its parameters select other servers.
func (s *Server) searchResponse(r *SearchRequest) (DeviceInfo, bool) {
	d := s.config.Device

	if r.Unsupported ||
		r.ProgrammingMode && d.Status&1 == 0 ||
		r.MAC != nil && *r.MAC != d.MAC ||
		r.Service.ID != 0 && !d.supportsVersion(r.Service) {
		return d, false
	}

	for _, dib := range r.DIBs {
		switch dib {
		case DIBTunnellingInfo:
			if len(s.config.Tunnels) > 0 {
				d.Tunnelling = s.tunnellingInfo()
			}

		case DIBExtendedInfo:
			d.Extended = &ExtendedDeviceInfo{MaxAPDU: MaxAPDU, Descriptor: RouterMask}
		}
	}

	return d, true
}

// tunnellingInfo tells which tunnels are in use.
func (s *Server) tunnellingInfo() *TunnellingInfo {
	t := &TunnellingInfo{MaxAPDU: MaxAPDU}

	for _, addr := range s.config.Tunnels {
		status := SlotUsable
		if s.secure == nil {
			status |= SlotAuthorized
		}

		if !s.tunnelInUse(addr) {
			status |= SlotFree
		}

		t.Slots = append(t.Slots, TunnelSlot{Address: addr, Status: status})
	}

	return t
}

func (s *Server) tunnelInUse(addr cemi.IndividualAddr) bool {
	for _, c := range s.conns {
		if c.kind == TunnelConnection && c.address == addr {
			return true
		}
	}

	return false
}

func (s *Server) freeTunnel() (cemi.IndividualAddr, bool) {
	for _, addr := range s.config.Tunnels {
		if !s.tunnelInUse(addr) {
			return addr, true
		}
	}

	return 0, false
}

// request handles a TUNNELLING_REQUEST, DEVICE_CONFIGURATION_REQUEST or ObjectServer request on a
// connection of the kind.
func (s *Server) request(now time.Time, from HPAI, kind ConnectionType, channel, seq uint8, msg queued) {
	ack := func(c *serverConn, status Status) {
		if from.Protocol == ProtocolTCP {
			return
		}

		var service Service
		switch kind {
		case TunnelConnection:
			service = &TunnellingAck{Channel: channel, Sequence: seq, Status: status}
		case DeviceManagementConnection:
			service = &DeviceConfigAck{Channel: channel, Sequence: seq, Status: status}
		default:
			service = &ObjectServerAck{Channel: channel, Sequence: seq, Status: status}
		}

		to := from
		if c != nil {
			to = c.data
		}

		s.reply(to, service)
	}

	c := s.peer(channel)
	if c == nil || c.kind != kind {
		ack(nil, StatusConnectionID)
		return
	}

	switch {
	case c.stream:
		c.seqRecv = seq
	case seq == c.seqRecv:
	case seq == c.seqRecv-1:
		ack(c, StatusOK) // our acknowledgement got lost
		return
	default:
		return
	}

	c.seqRecv++
	c.lastActive = now
	ack(c, StatusOK)

	if msg.msg == nil && msg.data == nil {
		return
	}

	if req, ok := msg.msg.(*cemi.LDataReq); ok && req.Source == 0 {
		req.Source = c.address
	}

	s.emit(ServerEvent{Kind: ServerMessage, Channel: channel, Type: kind, Address: c.address, Message: msg.msg, Data: msg.data, Session: c.sessionID(), User: c.user()})
}

func (s *Server) acked(now time.Time, kind ConnectionType, channel, seq uint8) {
	c := s.peer(channel)
	if c == nil || c.kind != kind || c.stream || c.pending == nil || seq != c.seqSend {
		return
	}

	// A negative acknowledgement drops the message too: the client will not take it.
	c.pending = nil
	c.queue = c.queue[1:]
	c.seqSend++
	c.lastActive = now

	s.flush(now, c)
}

// flush sends the next queued message of a connection if none is pending, or all of them over
// TCP.
func (s *Server) flush(now time.Time, c *serverConn) {
	for ; c.stream && len(c.queue) > 0; c.queue = c.queue[1:] {
		s.sendVia(c.session, c.data, Encode(c.request(c.queue[0])))
		c.seqSend++
	}

	if c.pending != nil || len(c.queue) == 0 {
		return
	}

	c.pending = Encode(c.request(c.queue[0]))
	c.sentAt = now
	c.repeated = false
	s.sendVia(c.session, c.data, c.pending)
}

func (s *Server) disconnect(c *serverConn) {
	s.sendVia(c.session, c.control, Encode(&DisconnectRequest{Channel: c.channel, Control: s.config.Control}))
	s.remove(c)
}

func (s *Server) remove(c *serverConn) {
	for i, other := range s.conns {
		if other == c {
			s.conns = append(s.conns[:i], s.conns[i+1:]...)
			break
		}
	}

	s.emit(ServerEvent{Kind: ServerClosed, Channel: c.channel, Type: c.kind, Address: c.address, Session: c.sessionID(), User: c.user()})
}

func (s *Server) conn(channel uint8) *serverConn {
	for _, c := range s.conns {
		if c.channel == channel {
			return c
		}
	}

	return nil
}

// peer returns the connection on a channel if it belongs to the session of the packet being
// handled.
func (s *Server) peer(channel uint8) *serverConn {
	if c := s.conn(channel); c != nil && c.session == s.current {
		return c
	}

	return nil
}

// request returns the request that carries a message on the connection.
func (c *serverConn) request(msg queued) Service {
	switch c.kind {
	case TunnelConnection:
		return &TunnellingRequest{Channel: c.channel, Sequence: c.seqSend, Message: msg.msg}
	case DeviceManagementConnection:
		return &DeviceConfigRequest{Channel: c.channel, Sequence: c.seqSend, Message: msg.msg}
	}

	return &ObjectServerRequest{Channel: c.channel, Sequence: c.seqSend, Message: msg.data}
}

func (c *serverConn) sessionID() uint16 {
	if c.session == nil {
		return 0
	}

	return c.session.id
}

func (c *serverConn) user() uint8 {
	if c.session == nil {
		return 0
	}

	return c.session.user
}

// reply answers the packet being handled, in its session.
func (s *Server) reply(to HPAI, service Service) {
	s.sendVia(s.current, to, Encode(service))
}

func (s *Server) emit(event ServerEvent) {
	s.events = append(s.events, event)
}
