package knxnet

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"io"
	"time"

	"github.com/shabunin/tiny-knx-go/cemi"
	"github.com/shabunin/tiny-knx-go/secure"
)

// TunnelConfig configures a Tunnel. Zero fields take the values of DefaultTunnelConfig.
type TunnelConfig struct {
	// Type is the type of the connection: TunnelConnection (the default), or
	// ObjectServerConnection to the ObjectServer of an IP BAOS, which carries messages of the
	// ObjectServer protocol (SendObjectServer) instead of cEMI messages.
	Type ConnectionType

	Layer TunnelLayer

	// ConnectTimeout is how long to wait for the CONNECT_RESPONSE.
	ConnectTimeout time.Duration

	// AckTimeout is how long to wait for a TUNNELLING_ACK before sending the request once more.
	AckTimeout time.Duration

	// HeartbeatInterval is the time between CONNECTIONSTATE_REQUESTs. Servers close connections
	// that have been silent for 120 seconds.
	HeartbeatInterval time.Duration

	// HeartbeatTimeout is how long to wait for a CONNECTIONSTATE_RESPONSE, and HeartbeatAttempts
	// how many requests go unanswered before the connection is considered lost.
	HeartbeatTimeout  time.Duration
	HeartbeatAttempts int

	// DisconnectTimeout is how long Disconnect waits for the DISCONNECT_RESPONSE.
	DisconnectTimeout time.Duration

	// Secure, if set, opens the tunnel in a secure session (KNX IP Secure). The heartbeat then
	// comes at least every 50 seconds, within the 60-second timeout of sessions.
	Secure *TunnelSecure
}

// TunnelSecure configures the secure session of a Tunnel.
type TunnelSecure struct {
	// User is the user ID: 2 and up for tunnelling users, 1 for the management user.
	User uint8

	// Password is the key of the user's password (secure.UserPasswordKey).
	Password [16]byte

	// DeviceAuth is the key of the server's device authentication code (secure.DeviceAuthKey).
	// If it is zero, the server is not checked.
	DeviceAuth [16]byte

	// Serial is the serial number of the client in its secure wrappers.
	Serial [6]byte

	// Random is the source of the session key pair. Nil means crypto/rand.
	Random io.Reader
}

// DefaultTunnelConfig contains the timings from the KNXnet/IP specification.
var DefaultTunnelConfig = TunnelConfig{
	Type:              TunnelConnection,
	Layer:             TunnelLinkLayer,
	ConnectTimeout:    10 * time.Second,
	AckTimeout:        time.Second,
	HeartbeatInterval: 60 * time.Second,
	HeartbeatTimeout:  10 * time.Second,
	HeartbeatAttempts: 3,
	DisconnectTimeout: 2 * time.Second,
}

func (cfg TunnelConfig) withDefaults() TunnelConfig {
	d := DefaultTunnelConfig

	if cfg.Type == 0 {
		cfg.Type = d.Type
	}

	if cfg.Layer == 0 {
		cfg.Layer = d.Layer
	}

	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = d.ConnectTimeout
	}

	if cfg.AckTimeout <= 0 {
		cfg.AckTimeout = d.AckTimeout
	}

	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = d.HeartbeatInterval
	}

	if cfg.HeartbeatTimeout <= 0 {
		cfg.HeartbeatTimeout = d.HeartbeatTimeout
	}

	if cfg.HeartbeatAttempts <= 0 {
		cfg.HeartbeatAttempts = d.HeartbeatAttempts
	}

	if cfg.DisconnectTimeout <= 0 {
		cfg.DisconnectTimeout = d.DisconnectTimeout
	}

	if cfg.Secure != nil {
		cfg.HeartbeatInterval = min(cfg.HeartbeatInterval, 50*time.Second)
	}

	return cfg
}

// These errors are reported by a Tunnel.
var (
	ErrNotConnected     = errors.New("knxnet: tunnel not connected")
	ErrBusy             = errors.New("knxnet: a tunnelling request is still unacknowledged")
	ErrConnectTimeout   = errors.New("knxnet: no response to connect request")
	ErrAckTimeout       = errors.New("knxnet: tunnelling request not acknowledged")
	ErrHeartbeatTimeout = errors.New("knxnet: no response to connection state request")
	ErrClosedByServer   = errors.New("knxnet: tunnel closed by server")
	ErrNoSession        = errors.New("knxnet: server has no free secure session")
	ErrDeviceAuth       = errors.New("knxnet: server failed device authentication")
)

// TunnelState is the state of a Tunnel.
type TunnelState uint8

// These are the tunnel states.
const (
	TunnelClosed TunnelState = iota
	TunnelConnecting
	TunnelConnected
	TunnelDisconnecting
)

// EventKind tells what an Event means.
type EventKind uint8

// These are the tunnel event kinds.
const (
	// EventSend asks the caller to send Packet to the server: to the data endpoint if ToData is
	// set, otherwise to the control endpoint.
	EventSend EventKind = iota

	// EventConnected reports an open tunnel. Address is the individual address the server
	// assigned to it.
	EventConnected

	// EventDisconnected reports that the tunnel was closed. Err is nil after Disconnect.
	EventDisconnected

	// EventMessage delivers a cEMI message from the server, e.g. *cemi.LDataInd or
	// *cemi.LDataCon, or on an ObjectServer connection the message of the ObjectServer protocol
	// in Data.
	EventMessage

	// EventAcked reports the server's answer to the last Send. Err is a Status if the server
	// rejected the request. The message is on its way to the bus; its L_Data.con follows as
	// an EventMessage.
	EventAcked
)

// An Event is an output of a Tunnel.
type Event struct {
	Kind    EventKind
	Packet  []byte              // for EventSend
	ToData  bool                // for EventSend
	Address cemi.IndividualAddr // for EventConnected
	Message cemi.Message        // for EventMessage
	Data    []byte              // for EventMessage on an ObjectServer connection
	Err     error               // for EventDisconnected and EventAcked
}

// A Tunnel is the client side of a KNXnet/IP tunnelling connection. It does no I/O: the caller
// passes received packets to HandlePacket, calls Tick when Deadline passes and carries out the
// events from NextEvent.
//
// The tunnel uses route-back HPAIs (0.0.0.0:0), so the server answers to the address that its
// packets come from. This works through NAT.
type Tunnel struct {
	config  TunnelConfig
	state   TunnelState
	channel uint8
	address cemi.IndividualAddr
	data    HPAI

	seqSend uint8
	seqRecv uint8

	pending  []byte // unacknowledged tunnelling request
	repeated bool   // pending has been sent twice

	timer      time.Time // response deadline of connect, disconnect or the pending request
	timerSet   bool
	heartbeat  time.Time // next CONNECTIONSTATE_REQUEST, or deadline of its response
	heartbeats int       // unanswered CONNECTIONSTATE_REQUESTs

	// The secure session: key is set once the server answered the SESSION_REQUEST.
	session       uint16
	key           cipher.Block
	private       *ecdh.PrivateKey
	public        [PublicKeySize]byte
	seqSecure     uint64 // sequence number of the next SECURE_WRAPPER
	seqSecureRecv uint64 // lowest valid sequence number from the server
	authenticated bool

	events []Event
	next   int
}

// NewTunnel returns a closed tunnel.
func NewTunnel(config TunnelConfig) *Tunnel {
	return &Tunnel{config: config.withDefaults()}
}

// State returns the state of the tunnel.
func (t *Tunnel) State() TunnelState {
	return t.state
}

// Address returns the individual address assigned by the server.
func (t *Tunnel) Address() cemi.IndividualAddr {
	return t.address
}

// DataEndpoint returns the server's data endpoint from the CONNECT_RESPONSE. A route-back HPAI
// means the control endpoint.
func (t *Tunnel) DataEndpoint() HPAI {
	return t.data
}

// NextEvent returns the next pending event.
func (t *Tunnel) NextEvent() (Event, bool) {
	if t.next == len(t.events) {
		t.events = t.events[:0]
		t.next = 0

		return Event{}, false
	}

	event := t.events[t.next]
	t.events[t.next] = Event{}
	t.next++

	return event, true
}

// Deadline returns when Tick must be called next.
func (t *Tunnel) Deadline() (time.Time, bool) {
	switch {
	case t.state == TunnelConnected && t.timerSet && t.timer.Before(t.heartbeat):
		return t.timer, true
	case t.state == TunnelConnected:
		return t.heartbeat, true
	case t.timerSet:
		return t.timer, true
	}

	return time.Time{}, false
}

// Connect sends a CONNECT_REQUEST.
func (t *Tunnel) Connect(now time.Time) {
	if t.state != TunnelClosed {
		return
	}

	*t = Tunnel{config: t.config, events: t.events, next: t.next}
	t.state = TunnelConnecting
	t.setTimer(now, t.config.ConnectTimeout)

	if t.config.Secure == nil {
		t.send(&ConnectRequest{Type: t.config.Type, Layer: t.config.Layer}, false)
	} else if err := t.requestSession(); err != nil {
		t.close(err)
	}
}

// Disconnect sends a DISCONNECT_REQUEST. EventDisconnected follows when the server answers or
// after DisconnectTimeout.
func (t *Tunnel) Disconnect(now time.Time) {
	switch t.state {
	case TunnelConnecting:
		t.close(nil)

	case TunnelConnected:
		t.state = TunnelDisconnecting
		t.send(&DisconnectRequest{Channel: t.channel}, false)
		t.setTimer(now, t.config.DisconnectTimeout)
	}
}

// Send sends a cEMI message, usually *cemi.LDataReq, to the server. Only one request may be
// unacknowledged at a time: wait for EventAcked before sending the next one.
func (t *Tunnel) Send(now time.Time, msg cemi.Message) error {
	return t.request(now, &TunnellingRequest{Channel: t.channel, Sequence: t.seqSend, Message: msg})
}

// SendObjectServer sends a message of the ObjectServer protocol on an ObjectServer connection, like
// Send.
func (t *Tunnel) SendObjectServer(now time.Time, message []byte) error {
	return t.request(now, &ObjectServerRequest{Channel: t.channel, Sequence: t.seqSend, Message: message})
}

func (t *Tunnel) request(now time.Time, s Service) error {
	if t.state != TunnelConnected {
		return ErrNotConnected
	}

	if t.pending != nil {
		return ErrBusy
	}

	t.pending = Encode(s)
	t.repeated = false
	t.out(t.pending, true)
	t.setTimer(now, t.config.AckTimeout)

	return nil
}

// Tick handles timeouts.
func (t *Tunnel) Tick(now time.Time) {
	if t.timerSet && !now.Before(t.timer) {
		t.timerSet = false

		switch t.state {
		case TunnelConnecting:
			t.close(ErrConnectTimeout)

		case TunnelDisconnecting:
			t.close(nil)

		case TunnelConnected:
			if t.pending != nil && !t.repeated {
				t.repeated = true
				t.out(t.pending, true)
				t.setTimer(now, t.config.AckTimeout)
			} else if t.pending != nil {
				t.fail(ErrAckTimeout)
				return
			}
		}
	}

	if t.state == TunnelConnected && !now.Before(t.heartbeat) {
		if t.heartbeats == t.config.HeartbeatAttempts {
			t.fail(ErrHeartbeatTimeout)
			return
		}

		t.heartbeats++
		t.send(&ConnectionStateRequest{Channel: t.channel}, false)
		t.heartbeat = now.Add(t.config.HeartbeatTimeout)
	}
}

// HandlePacket processes a packet from the server. Packets for other channels or services are
// ignored.
func (t *Tunnel) HandlePacket(now time.Time, packet []byte) {
	s, err := Decode(packet)
	if err != nil {
		return
	}

	if t.config.Secure != nil {
		if s = t.unwrap(now, s); s == nil {
			return
		}
	}

	switch s := s.(type) {
	case *ConnectResponse:
		if t.state != TunnelConnecting {
			return
		}

		if s.Status != StatusOK {
			t.close(s.Status)
			return
		}

		t.state = TunnelConnected
		t.channel = s.Channel
		t.address = s.Address
		t.data = s.Data
		t.timerSet = false
		t.heartbeat = now.Add(t.config.HeartbeatInterval)
		t.emit(Event{Kind: EventConnected, Address: s.Address})

	case *ConnectionStateResponse:
		if t.state != TunnelConnected || s.Channel != t.channel || t.heartbeats == 0 {
			return
		}

		if s.Status != StatusOK {
			t.fail(s.Status)
			return
		}

		t.heartbeats = 0
		t.heartbeat = now.Add(t.config.HeartbeatInterval)

	case *DisconnectRequest:
		if t.state == TunnelClosed || t.state == TunnelConnecting || s.Channel != t.channel {
			return
		}

		t.send(&DisconnectResponse{Channel: t.channel}, false)

		if t.state == TunnelDisconnecting {
			t.close(nil)
		} else {
			t.close(ErrClosedByServer)
		}

	case *DisconnectResponse:
		if t.state == TunnelDisconnecting && s.Channel == t.channel {
			t.close(nil)
		}

	case *TunnellingAck:
		t.acked(TunnelConnection, s.Channel, s.Sequence, s.Status)

	case *ObjectServerAck:
		t.acked(ObjectServerConnection, s.Channel, s.Sequence, s.Status)

	case *TunnellingRequest:
		t.received(TunnelConnection, s.Channel, s.Sequence, Event{Kind: EventMessage, Message: s.Message})

	case *ObjectServerRequest:
		t.received(ObjectServerConnection, s.Channel, s.Sequence, Event{Kind: EventMessage, Data: s.Message})
	}
}

func (t *Tunnel) acked(kind ConnectionType, channel, seq uint8, status Status) {
	if t.state != TunnelConnected || kind != t.config.Type || channel != t.channel || t.pending == nil ||
		seq != t.seqSend {
		return
	}

	t.pending = nil
	t.timerSet = false

	if status != StatusOK {
		t.emit(Event{Kind: EventAcked, Err: status})
		return
	}

	t.seqSend++
	t.emit(Event{Kind: EventAcked})
}

func (t *Tunnel) received(kind ConnectionType, channel, seq uint8, event Event) {
	if t.state != TunnelConnected || kind != t.config.Type || channel != t.channel {
		return
	}

	switch seq {
	case t.seqRecv:
		t.ack(seq)
		t.seqRecv++
		t.emit(event)

	case t.seqRecv - 1:
		t.ack(seq) // our acknowledgement got lost
	}
}

func (t *Tunnel) ack(seq uint8) {
	if t.config.Type == ObjectServerConnection {
		t.send(&ObjectServerAck{Channel: t.channel, Sequence: seq}, true)
		return
	}

	t.send(&TunnellingAck{Channel: t.channel, Sequence: seq}, true)
}

// fail closes a connection after an error, telling the server.
func (t *Tunnel) fail(err error) {
	t.send(&DisconnectRequest{Channel: t.channel}, false)
	t.close(err)
}

func (t *Tunnel) close(err error) {
	if t.key != nil {
		t.send(&SessionStatus{Code: SessionClose}, false)
		t.key = nil
	}

	t.state = TunnelClosed
	t.pending = nil
	t.timerSet = false
	t.emit(Event{Kind: EventDisconnected, Err: err})
}

func (t *Tunnel) send(s Service, toData bool) {
	t.out(Encode(s), toData)
}

// out sends a packet, in a SECURE_WRAPPER once there is a session key.
func (t *Tunnel) out(packet []byte, toData bool) {
	if t.key != nil {
		packet = Wrap(t.key, t.session, secure.Nonce{Seq: t.seqSecure, Serial: t.config.Secure.Serial}, packet)
		t.seqSecure++
	}

	t.emit(Event{Kind: EventSend, Packet: packet, ToData: toData})
}

// requestSession starts the secure session with a SESSION_REQUEST.
func (t *Tunnel) requestSession() error {
	random := t.config.Secure.Random
	if random == nil {
		random = rand.Reader
	}

	var b [32]byte
	if _, err := io.ReadFull(random, b[:]); err != nil {
		return err
	}

	private, err := ecdh.X25519().NewPrivateKey(b[:])
	if err != nil {
		return err
	}

	t.private = private
	copy(t.public[:], private.PublicKey().Bytes())
	t.send(&SessionRequest{PublicKey: t.public}, false)

	return nil
}

// unwrap handles the packets of the secure session. It returns the packet in a SECURE_WRAPPER for
// the tunnel, or nil. Plain packets other than SESSION_RESPONSE are ignored.
func (t *Tunnel) unwrap(now time.Time, s Service) Service {
	switch s := s.(type) {
	case *SessionResponse:
		if t.state == TunnelConnecting && t.key == nil {
			t.sessionResponse(s)
		}

	case *SecureWrapper:
		if t.key == nil || s.Session != t.session || s.Nonce.Seq < t.seqSecureRecv {
			return nil
		}

		packet, err := s.Open(t.key)
		if err != nil {
			return nil
		}

		t.seqSecureRecv = s.Nonce.Seq + 1

		inner, err := Decode(packet)
		if err != nil {
			return nil
		}

		if status, ok := inner.(*SessionStatus); ok {
			t.sessionStatus(now, status.Code)
			return nil
		}

		if t.authenticated {
			return inner
		}
	}

	return nil
}

// sessionResponse checks the server and authenticates the user.
func (t *Tunnel) sessionResponse(r *SessionResponse) {
	cfg := t.config.Secure

	if r.Session == 0 {
		t.close(ErrNoSession)
		return
	}

	if cfg.DeviceAuth != [16]byte{} {
		b, _ := aes.NewCipher(cfg.DeviceAuth[:])
		mac := SessionResponseMAC(b, r.Session, &t.public, &r.PublicKey)

		if subtle.ConstantTimeCompare(mac[:], r.MAC[:]) != 1 {
			t.close(ErrDeviceAuth)
			return
		}
	}

	peer, err := ecdh.X25519().NewPublicKey(r.PublicKey[:])
	if err != nil {
		t.close(ErrDeviceAuth)
		return
	}

	shared, err := t.private.ECDH(peer)
	if err != nil {
		t.close(ErrDeviceAuth)
		return
	}

	key := secure.SessionKey(shared)
	t.key, _ = aes.NewCipher(key[:])
	t.session = r.Session

	password, _ := aes.NewCipher(cfg.Password[:])
	t.send(&SessionAuthenticate{User: cfg.User, MAC: SessionAuthenticateMAC(password, cfg.User, &t.public, &r.PublicKey)}, false)
}

// sessionStatus opens the tunnel once the user is authenticated, and closes it when the session
// fails or ends.
func (t *Tunnel) sessionStatus(now time.Time, code SessionCode) {
	switch {
	case code == SessionKeepAlive:

	case code == SessionAuthenticated && t.state == TunnelConnecting && !t.authenticated:
		t.authenticated = true
		t.send(&ConnectRequest{Type: t.config.Type, Layer: t.config.Layer}, false)
		t.setTimer(now, t.config.ConnectTimeout)

	case t.state != TunnelClosed:
		t.key = nil // the session is gone, nothing more to send in it
		t.close(code)
	}
}

func (t *Tunnel) setTimer(now time.Time, d time.Duration) {
	t.timer = now.Add(d)
	t.timerSet = true
}

func (t *Tunnel) emit(event Event) {
	t.events = append(t.events, event)
}
