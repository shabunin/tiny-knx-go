package secure

import (
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
)

// KNX IP Secure (KNX 03_08_09, AN159) protects KNXnet/IP packets with the same AES-128 CCM as
// Data Secure, with a 16-byte MAC. A secure session starts with an X25519 key exchange; the
// server authenticates the exchange with its device authentication code, the client with the
// password of a user. The session key then protects every packet in a SECURE_WRAPPER.
//
// Package knxnet builds the packets; the functions here do the cryptography.

// UserPasswordKey returns the key that the password of a tunnelling or management user stands
// for, as ETS writes it into the interface.
func UserPasswordKey(password string) [16]byte {
	return deriveKey(password, "user-password.1.secure.ip.knx.org")
}

// DeviceAuthKey returns the key that the device authentication code of an interface stands for.
func DeviceAuthKey(code string) [16]byte {
	return deriveKey(code, "device-authentication-code.1.secure.ip.knx.org")
}

// KeyringPasswordKey returns the key that protects the keys in a keyring file (.knxkeys) that
// ETS exported with the password.
func KeyringPasswordKey(password string) [16]byte {
	return deriveKey(password, "1.keyring.ets.knx.org")
}

// deriveKey is PBKDF2 with HMAC-SHA256 and 65536 iterations, the first 16 bytes. Passwords are
// Latin-1 for the users and the device, which matches UTF-8 for ASCII.
func deriveKey(password, salt string) [16]byte {
	prf := hmac.New(sha256.New, []byte(password))
	prf.Write([]byte(salt))
	prf.Write([]byte{0, 0, 0, 1})

	u := prf.Sum(nil)
	var t [sha256.Size]byte
	copy(t[:], u)

	for i := 1; i < 65536; i++ {
		prf.Reset()
		prf.Write(u)
		u = prf.Sum(u[:0])

		for j := range t {
			t[j] ^= u[j]
		}
	}

	var key [16]byte
	copy(key[:], t[:])

	return key
}

// SessionKey returns the session key for the shared secret of the X25519 key exchange.
func SessionKey(shared []byte) [16]byte {
	sum := sha256.Sum256(shared)

	var key [16]byte
	copy(key[:], sum[:])

	return key
}

// HandshakeMAC returns the MAC of SESSION_RESPONSE (with the device authentication key) and
// SESSION_AUTHENTICATE (with the key of the user). ad is the packet up to the MAC, with the XOR of
// both public keys instead of the public key of SESSION_RESPONSE, or after the user ID of
// SESSION_AUTHENTICATE.
func HandshakeMAC(b cipher.Block, ad []byte) [16]byte {
	var b0, ctr0 [16]byte
	ctr0[14] = 0xFF

	tag := cbcMAC(b, &b0, ad, nil)
	newStream(b, &ctr0).xor(tag[:])

	return tag
}

// CheckHandshakeMAC reports whether mac is the HandshakeMAC of ad.
func CheckHandshakeMAC(b cipher.Block, ad []byte, mac [16]byte) bool {
	want := HandshakeMAC(b, ad)
	return subtle.ConstantTimeCompare(want[:], mac[:]) == 1
}

// Nonce identifies a SECURE_WRAPPER: the sequence number of the sender in the session (or the
// timer value of secure routing), the serial number of the sender and the message tag.
type Nonce struct {
	Seq    uint64
	Serial [6]byte
	Tag    uint16
}

func (n Nonce) block(last uint16) *[16]byte {
	var b [16]byte
	put48(b[:], n.Seq)
	copy(b[6:], n.Serial[:])
	binary.BigEndian.PutUint16(b[12:], n.Tag)
	binary.BigEndian.PutUint16(b[14:], last)

	return &b
}

// SealPacket encrypts a KNXnet/IP packet for a SECURE_WRAPPER with the session key. ad is the
// header of the SECURE_WRAPPER followed by the session ID.
func SealPacket(b cipher.Block, n Nonce, ad, packet []byte) (data []byte, mac [16]byte) {
	mac = cbcMAC(b, n.block(uint16(len(packet))), ad, packet)

	data = append([]byte(nil), packet...)
	s := newStream(b, n.block(0xFF00))
	s.xor(mac[:])
	s.xor(data)

	return data, mac
}

// OpenPacket checks and decrypts the packet of a SECURE_WRAPPER.
func OpenPacket(b cipher.Block, n Nonce, ad, data []byte, mac [16]byte) ([]byte, error) {
	packet := append([]byte(nil), data...)
	s := newStream(b, n.block(0xFF00))
	s.xor(mac[:])
	s.xor(packet)

	want := cbcMAC(b, n.block(uint16(len(packet))), ad, packet)
	if subtle.ConstantTimeCompare(want[:], mac[:]) != 1 {
		return nil, ErrMAC
	}

	return packet, nil
}
