package knxnet

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/subtle"
	"io"
	"time"

	"github.com/shabunin/tiny-knx-go/secure"
)

// SecureConfig makes a Server a KNX IP Secure server: tunnelling and device management then need
// a secure session, and plain connection requests are rejected.
type SecureConfig struct {
	// DeviceAuth is the key of the device authentication code (secure.DeviceAuthKey), with
	// which the server proves itself to clients.
	DeviceAuth [16]byte

	// Users maps user IDs to the keys of their passwords (secure.UserPasswordKey). User 1 is the
	// management user, the only one allowed to open device management connections; users 2 and
	// up may open tunnels.
	Users map[uint8][16]byte

	// SessionTimeout closes sessions without a valid packet for this long. Zero means 60 seconds.
	SessionTimeout time.Duration

	// MaxSessions limits the open sessions. Zero means 16.
	MaxSessions int

	// Random is the source of the session key pairs. Nil means crypto/rand.
	Random io.Reader
}

func (c *SecureConfig) withDefaults() *SecureConfig {
	d := *c

	if d.SessionTimeout <= 0 {
		d.SessionTimeout = 60 * time.Second
	}

	if d.MaxSessions <= 0 {
		d.MaxSessions = 16
	}

	if d.Random == nil {
		d.Random = rand.Reader
	}

	return &d
}

// A secure session of a Server.
type session struct {
	id         uint16
	key        cipher.Block
	control    HPAI // client endpoint for the session status
	user       uint8
	seqSend    uint64
	seqRecv    uint64 // lowest sequence number still valid
	lastActive time.Time

	client, server [PublicKeySize]byte
}

// sessionRequest answers a SESSION_REQUEST with a new session.
func (s *Server) sessionRequest(now time.Time, control HPAI, r *SessionRequest) {
	if len(s.sessions) >= s.secure.MaxSessions {
		s.sendVia(nil, control, Encode(&SessionResponse{}))
		return
	}

	var random [32]byte
	if _, err := io.ReadFull(s.secure.Random, random[:]); err != nil {
		return
	}

	curve := ecdh.X25519()

	private, err := curve.NewPrivateKey(random[:])
	if err != nil {
		return
	}

	peer, err := curve.NewPublicKey(r.PublicKey[:])
	if err != nil {
		return
	}

	shared, err := private.ECDH(peer)
	if err != nil {
		return
	}

	key := secure.SessionKey(shared)
	block, _ := aes.NewCipher(key[:])

	sess := &session{key: block, control: control, lastActive: now, client: r.PublicKey}
	copy(sess.server[:], private.PublicKey().Bytes())

	for s.lastSession++; s.lastSession == 0 || s.session(s.lastSession) != nil; s.lastSession++ {
	}

	sess.id = s.lastSession
	s.sessions = append(s.sessions, sess)

	s.sendVia(nil, control, Encode(&SessionResponse{
		Session:   sess.id,
		PublicKey: sess.server,
		MAC:       SessionResponseMAC(s.deviceAuth, sess.id, &sess.client, &sess.server),
	}))
}

// wrapped handles a SECURE_WRAPPER: the session handshake and status, and the packets of the
// connections in the session.
func (s *Server) wrapped(now time.Time, from HPAI, w *SecureWrapper) {
	sess := s.session(w.Session)
	if sess == nil || w.Nonce.Seq < sess.seqRecv {
		return
	}

	packet, err := w.Open(sess.key)
	if err != nil {
		return
	}

	sess.seqRecv = w.Nonce.Seq + 1
	sess.lastActive = now

	service, err := Decode(packet)
	if err != nil {
		return
	}

	switch r := service.(type) {
	case *SessionAuthenticate:
		if sess.user != 0 {
			return
		}

		code := SessionAuthFailed
		if key, ok := s.secure.Users[r.User]; ok && r.User != 0 {
			password, _ := aes.NewCipher(key[:])
			mac := SessionAuthenticateMAC(password, r.User, &sess.client, &sess.server)
			if subtle.ConstantTimeCompare(mac[:], r.MAC[:]) == 1 {
				code = SessionAuthenticated
			}
		}

		s.sendVia(sess, sess.control, Encode(&SessionStatus{Code: code}))
		s.emit(ServerEvent{Kind: ServerSecure, Session: sess.id, User: r.User, Code: code})

		if code == SessionAuthenticated {
			sess.user = r.User
		} else {
			s.closeSession(sess, 0)
		}

	case *SessionStatus:
		if r.Code == SessionClose {
			s.closeSession(sess, SessionClose)
		}

	case *SecureWrapper, *SessionRequest, *SessionResponse:
		// Sessions do not nest.

	default:
		if sess.user == 0 {
			s.sendVia(sess, sess.control, Encode(&SessionStatus{Code: SessionUnauthenticated}))
			return
		}

		s.current = sess
		s.handle(now, from, service)
		s.current = nil
	}
}

// closeSession removes a session and its connections. A code is reported in a ServerSecure event.
func (s *Server) closeSession(sess *session, code SessionCode) {
	for i := 0; i < len(s.conns); i++ {
		if c := s.conns[i]; c.session == sess {
			s.remove(c)
			i--
		}
	}

	for i, other := range s.sessions {
		if other == sess {
			s.sessions = append(s.sessions[:i], s.sessions[i+1:]...)
			break
		}
	}

	if code != 0 {
		s.emit(ServerEvent{Kind: ServerSecure, Session: sess.id, User: sess.user, Code: code})
	}
}

func (s *Server) session(id uint16) *session {
	for _, sess := range s.sessions {
		if sess.id == id {
			return sess
		}
	}

	return nil
}

// sendVia sends a packet, in a SECURE_WRAPPER if the session is not nil.
func (s *Server) sendVia(sess *session, to HPAI, packet []byte) {
	if sess != nil {
		packet = Wrap(sess.key, sess.id, secure.Nonce{Seq: sess.seqSend, Serial: s.config.Device.SerialNumber}, packet)
		sess.seqSend++
	}

	s.emit(ServerEvent{Kind: ServerSend, Packet: packet, To: to})
}
