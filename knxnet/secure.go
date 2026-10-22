package knxnet

import (
	"crypto/cipher"
	"encoding/binary"

	"github.com/shabunin/tiny-knx-go/secure"
)

// KNX IP Secure (KNX 03_08_09): a client opens a secure session with SESSION_REQUEST and
// SESSION_AUTHENTICATE, and from then on every packet of the session travels encrypted in a
// SECURE_WRAPPER, including the packets of the tunnelling connection inside it. Package secure
// does the cryptography.

// PublicKeySize is the size of the X25519 public keys of the session handshake.
const PublicKeySize = 32

// SecureWrapper carries an encrypted packet of a secure session.
type SecureWrapper struct {
	Session uint16
	Nonce   secure.Nonce
	Data    []byte // the encrypted packet
	MAC     [16]byte
}

func (*SecureWrapper) ServiceID() ServiceID { return SecureWrapperID }

func (s *SecureWrapper) appendBody(b []byte) []byte {
	b = binary.BigEndian.AppendUint16(b, s.Session)
	b = append(b, byte(s.Nonce.Seq>>40), byte(s.Nonce.Seq>>32), byte(s.Nonce.Seq>>24),
		byte(s.Nonce.Seq>>16), byte(s.Nonce.Seq>>8), byte(s.Nonce.Seq))
	b = append(b, s.Nonce.Serial[:]...)
	b = binary.BigEndian.AppendUint16(b, s.Nonce.Tag)
	b = append(b, s.Data...)

	return append(b, s.MAC[:]...)
}

// wrapperOverhead is the size of a SECURE_WRAPPER without the packet in it.
const wrapperOverhead = headerSize + 2 + 6 + 6 + 2 + 16

// wrapperAD returns the associated data of a SECURE_WRAPPER: its header and the session ID.
func wrapperAD(session uint16, packetSize int) []byte {
	ad := make([]byte, headerSize+2)
	ad[0] = headerSize
	ad[1] = 0x10
	binary.BigEndian.PutUint16(ad[2:], uint16(SecureWrapperID))
	binary.BigEndian.PutUint16(ad[4:], uint16(wrapperOverhead+packetSize))
	binary.BigEndian.PutUint16(ad[6:], session)

	return ad
}

// Wrap returns a SECURE_WRAPPER packet that carries the packet, encrypted with the session key.
func Wrap(key cipher.Block, session uint16, n secure.Nonce, packet []byte) []byte {
	data, mac := secure.SealPacket(key, n, wrapperAD(session, len(packet)), packet)
	return Encode(&SecureWrapper{Session: session, Nonce: n, Data: data, MAC: mac})
}

// Open checks and decrypts the packet in the wrapper.
func (s *SecureWrapper) Open(key cipher.Block) ([]byte, error) {
	return secure.OpenPacket(key, s.Nonce, wrapperAD(s.Session, len(s.Data)), s.Data, s.MAC)
}

// SessionRequest starts a secure session. Control is the endpoint for the answers.
type SessionRequest struct {
	Control   HPAI
	PublicKey [PublicKeySize]byte
}

func (*SessionRequest) ServiceID() ServiceID { return SessionRequestID }

func (s *SessionRequest) appendBody(b []byte) []byte {
	return append(appendHPAI(b, s.Control), s.PublicKey[:]...)
}

// SessionResponse answers a SessionRequest with the session ID and the public key of the server,
// authenticated with its device authentication code. Session 0 means that the server has no
// free session; the other fields are then missing.
type SessionResponse struct {
	Session   uint16
	PublicKey [PublicKeySize]byte
	MAC       [16]byte
}

func (*SessionResponse) ServiceID() ServiceID { return SessionResponseID }

func (s *SessionResponse) appendBody(b []byte) []byte {
	b = binary.BigEndian.AppendUint16(b, s.Session)
	if s.Session == 0 {
		return b
	}

	return append(append(b, s.PublicKey[:]...), s.MAC[:]...)
}

// SessionResponseMAC returns the MAC of a SessionResponse for the public key of the client.
func SessionResponseMAC(deviceAuth cipher.Block, session uint16, client, server *[PublicKeySize]byte) [16]byte {
	ad := []byte{headerSize, 0x10, 0x09, 0x52, 0, 0x38}
	ad = binary.BigEndian.AppendUint16(ad, session)

	return secure.HandshakeMAC(deviceAuth, appendXOR(ad, client, server))
}

// SessionAuthenticate authenticates the client of a secure session as a user, with a MAC made
// with the key of the user's password. It travels in a SecureWrapper.
type SessionAuthenticate struct {
	User uint8 // 1 is the management user, 2 and up are tunnelling users
	MAC  [16]byte
}

func (*SessionAuthenticate) ServiceID() ServiceID { return SessionAuthenticateID }

func (s *SessionAuthenticate) appendBody(b []byte) []byte {
	return append(append(b, 0, s.User), s.MAC[:]...)
}

// SessionAuthenticateMAC returns the MAC of a SessionAuthenticate.
func SessionAuthenticateMAC(password cipher.Block, user uint8, client, server *[PublicKeySize]byte) [16]byte {
	ad := []byte{headerSize, 0x10, 0x09, 0x53, 0, 0x18, 0, user}
	return secure.HandshakeMAC(password, appendXOR(ad, client, server))
}

func appendXOR(b []byte, x, y *[PublicKeySize]byte) []byte {
	for i := range x {
		b = append(b, x[i]^y[i])
	}

	return b
}

// SessionCode is the status of a secure session.
type SessionCode uint8

// These are the session status codes.
const (
	SessionAuthenticated   SessionCode = 0x00
	SessionAuthFailed      SessionCode = 0x01
	SessionUnauthenticated SessionCode = 0x02
	SessionTimeout         SessionCode = 0x03
	SessionKeepAlive       SessionCode = 0x04
	SessionClose           SessionCode = 0x05
)

var sessionCodeText = map[SessionCode]string{
	SessionAuthenticated:   "authenticated",
	SessionAuthFailed:      "authentication failed",
	SessionUnauthenticated: "not authenticated",
	SessionTimeout:         "session timeout",
	SessionKeepAlive:       "keep alive",
	SessionClose:           "session closed",
}

func (c SessionCode) String() string {
	if text, ok := sessionCodeText[c]; ok {
		return text
	}

	return "session status " + Status(c).String()
}

func (c SessionCode) Error() string {
	return "knxnet: " + c.String()
}

// SessionStatus reports the status of a secure session, in a SecureWrapper.
type SessionStatus struct {
	Code SessionCode
}

func (*SessionStatus) ServiceID() ServiceID { return SessionStatusID }

func (s *SessionStatus) appendBody(b []byte) []byte {
	return append(b, byte(s.Code), 0)
}

func (r *reader) secureWrapper() *SecureWrapper {
	if len(r.data) < wrapperOverhead-headerSize {
		r.fail(ErrTruncated)
		return nil
	}

	s := &SecureWrapper{Session: r.uint16()}

	seq := r.take(6)
	s.Nonce.Seq = uint64(seq[0])<<40 | uint64(seq[1])<<32 | uint64(binary.BigEndian.Uint32(seq[2:]))
	copy(s.Nonce.Serial[:], r.take(6))
	s.Nonce.Tag = r.uint16()

	s.Data = append([]byte(nil), r.take(len(r.data)-len(s.MAC))...)
	copy(s.MAC[:], r.take(len(s.MAC)))

	return s
}

func (r *reader) sessionRequest() *SessionRequest {
	s := &SessionRequest{Control: r.hpai()}
	copy(s.PublicKey[:], r.take(PublicKeySize))

	return s
}

func (r *reader) sessionResponse() *SessionResponse {
	s := &SessionResponse{Session: r.uint16()}
	if s.Session == 0 && len(r.data) == 0 {
		return s
	}

	copy(s.PublicKey[:], r.take(PublicKeySize))
	copy(s.MAC[:], r.take(len(s.MAC)))

	return s
}

func (r *reader) sessionAuthenticate() *SessionAuthenticate {
	s := &SessionAuthenticate{User: r.skip(1).byte()}
	copy(s.MAC[:], r.take(len(s.MAC)))

	return s
}
