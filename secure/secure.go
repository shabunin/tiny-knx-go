// Package secure implements KNX Data Secure, the secure application layer service A_SecureService
// (APCI 0x3F1) that authenticates, and usually encrypts, an APDU with AES-128 in CCM mode.
//
// A secure APDU carries a security control field (SCF), the 48-bit sequence number of its sender,
// the protected APDU and a 4-byte message authentication code (MAC). The MAC also covers the
// addresses, the address type and the transport layer control of the frame, so a secure APDU is
// valid only in the frame it was made for (see Frame). Values follow KNX 03_03_07 §5 and AN158.
//
// The CCM here is the KNX variant: the length of the associated data, the associated data and the
// payload are padded together, not separately as in RFC 3610.
//
// The functions work on the secure ASDU, the bytes after the APCI: Seal and the SealSync functions
// make one, Parse splits one, and the Open functions check and decrypt it. Keys are passed as a
// cipher.Block, e.g. from aes.NewCipher, so that a caller can keep one per key.
package secure

import (
	"crypto/cipher"
	"crypto/subtle"
	"errors"

	"github.com/shabunin/tiny-knx-go/apdu"
	"github.com/shabunin/tiny-knx-go/cemi"
)

// These are the errors of Parse and the Open functions.
var (
	ErrInvalid = errors.New("secure: invalid secure APDU")
	ErrMAC     = errors.New("secure: MAC mismatch")
)

// Sizes of the parts of a secure ASDU.
const (
	SeqSize = 6
	MACSize = 4

	// MaxSeq is the largest sequence number.
	MaxSeq = 1<<48 - 1
)

// Control is the security control field (SCF), the first byte of a secure ASDU: the flags below
// combined with one of the services.
type Control uint8

// These are the flags of the security control field.
const (
	// ToolAccess means the APDU is protected with the tool key of the device, as ETS does for
	// management, instead of a group key.
	ToolAccess Control = 0x80

	// Confidential means the APDU is encrypted (algorithm "CCM with encryption"). Without it, it
	// is only authenticated and travels in plain text.
	Confidential Control = 0x10

	// SystemBroadcast means the APDU is sent as a system broadcast.
	SystemBroadcast Control = 0x08
)

// These are the services of the security control field.
const (
	Data         Control = 0 // S-A_Data, a protected APDU
	SyncRequest  Control = 2 // S-A_Sync_Req, asks for the sequence numbers of the receiver
	SyncResponse Control = 3 // S-A_Sync_Res, answers a sync request
)

// Service returns the service of the security control field.
func (c Control) Service() Control {
	return c & 7
}

// Frame holds the parts of a frame that a secure APDU is bound to.
type Frame struct {
	Source      cemi.IndividualAddr
	Destination uint16

	// Group is true when the destination is a group address, including the broadcast address 0.
	Group bool

	// Format is the extended frame format of extended frames, 0 for standard frames.
	Format uint8

	// TPCI holds the transport layer control bits of the first byte of the TPDU (with the two
	// APCI bits zero): 0 for T_Data_Group, T_Data_Broadcast and T_Data_Individual, 0x40 | seq<<2
	// for T_Data_Connected.
	TPCI uint8
}

// FrameOf returns the parts of a frame that a secure APDU in it is bound to.
func FrameOf(ld *cemi.LData) Frame {
	f := Frame{
		Source:      ld.Source,
		Destination: ld.Destination,
		Group:       ld.Control2.IsGroupAddr(),
		Format:      uint8(ld.Control2) & 0x0F,
	}

	if app, ok := ld.Data.(*cemi.AppData); ok && app.Numbered {
		f.TPCI = 0x40 | (app.SeqNumber&15)<<2
	}

	return f
}

// Message is a secure ASDU split into its parts.
type Message struct {
	Control Control

	// Seq is the sequence number of the sender. In a sync response, it holds a random value XOR
	// the challenge of the request; OpenSyncResponse undoes this.
	Seq uint64

	// Serial is the serial number of the addressed device in a sync request, or zero.
	Serial apdu.SerialNumber

	payload []byte // the APDU, encrypted if the control is Confidential
	mac     [MACSize]byte
}

// Parse splits a secure ASDU into its parts without checking the MAC, so that the caller can pick
// the key and check the sequence number first.
func Parse(asdu []byte) (Message, error) {
	if len(asdu) < 1+SeqSize+MACSize {
		return Message{}, ErrInvalid
	}

	m := Message{Control: Control(asdu[0]), Seq: get48(asdu[1:])}

	// Only the algorithms "CCM authentication" (0) and "CCM with encryption" (1) exist, and the
	// sync services are always encrypted.
	switch {
	case asdu[0]>>4&7 > 1:
		return Message{}, ErrInvalid
	case m.Control.Service() == Data:
	case m.Control.Service() == SyncRequest || m.Control.Service() == SyncResponse:
		if m.Control&Confidential == 0 {
			return Message{}, ErrInvalid
		}
	default:
		return Message{}, ErrInvalid
	}

	rest := asdu[1+SeqSize:]
	if m.Control.Service() == SyncRequest {
		if len(rest) < len(m.Serial)+MACSize {
			return Message{}, ErrInvalid
		}

		copy(m.Serial[:], rest)
		rest = rest[len(m.Serial):]
	}

	m.payload = rest[:len(rest)-MACSize]
	copy(m.mac[:], rest[len(rest)-MACSize:])

	return m, nil
}

// Seal returns the secure ASDU of an S-A_Data that protects the APDU (its APCI in two bytes,
// followed by its data; see Plain) for the frame f. The control is Data with any flags; seq is
// the next sequence number of the sender and must not exceed MaxSeq.
func Seal(b cipher.Block, f Frame, c Control, seq uint64, apdu []byte) []byte {
	return seal(b, f, c&^7|Data, seq, seq, nil, apdu)
}

// Open checks an S-A_Data for the frame f and returns its plain APDU.
func (m Message) Open(b cipher.Block, f Frame) ([]byte, error) {
	if m.Control.Service() != Data {
		return nil, ErrInvalid
	}

	return m.open(b, f, m.Seq, nil)
}

// SealSyncRequest returns the secure ASDU of a sync request that asks a device for its sequence
// numbers. seq is the next sequence number of the sender, serial addresses a device by its
// serial number (or is zero) and challenge is a random 48-bit value the response must repeat.
func SealSyncRequest(b cipher.Block, f Frame, c Control, seq uint64, serial apdu.SerialNumber, challenge uint64) []byte {
	var plain [SeqSize]byte
	put48(plain[:], challenge)

	return seal(b, f, c&^7|Confidential|SyncRequest, seq, seq, &serial, plain[:])
}

// OpenSyncRequest checks a sync request for the frame f and returns its challenge.
func (m Message) OpenSyncRequest(b cipher.Block, f Frame) (uint64, error) {
	if m.Control.Service() != SyncRequest {
		return 0, ErrInvalid
	}

	plain, err := m.open(b, f, m.Seq, m.Serial[:])
	if err != nil {
		return 0, err
	} else if len(plain) != SeqSize {
		return 0, ErrInvalid
	}

	return get48(plain), nil
}

// SealSyncResponse returns the secure ASDU of a sync response to a request with the challenge.
// seq is the next sequence number of the sender and remoteSeq the next one it expects from the
// requester. random is a fresh random 48-bit value that takes the place of the sequence number.
func SealSyncResponse(b cipher.Block, f Frame, c Control, random, challenge, seq, remoteSeq uint64) []byte {
	var plain [2 * SeqSize]byte
	put48(plain[:], seq)
	put48(plain[SeqSize:], remoteSeq)

	return seal(b, f, c&^7|Confidential|SyncResponse, random, random^challenge, nil, plain[:])
}

// OpenSyncResponse checks a sync response for the frame f to the request with the challenge, and
// returns the next sequence number of its sender and the next one the sender expects from us.
func (m Message) OpenSyncResponse(b cipher.Block, f Frame, challenge uint64) (seq, remoteSeq uint64, err error) {
	if m.Control.Service() != SyncResponse {
		return 0, 0, ErrInvalid
	}

	plain, err := m.open(b, f, (m.Seq^challenge)&MaxSeq, nil)
	if err != nil {
		return 0, 0, err
	} else if len(plain) != 2*SeqSize {
		return 0, 0, ErrInvalid
	}

	return get48(plain), get48(plain[SeqSize:]), nil
}

// seal makes a secure ASDU. nonce goes into the CCM blocks and field into the sequence number
// field; they differ only in a sync response. A sync request also authenticates the serial.
func seal(b cipher.Block, f Frame, c Control, nonce, field uint64, serial *apdu.SerialNumber, plain []byte) []byte {
	asdu := make([]byte, 1+SeqSize, 1+SeqSize+len(apdu.SerialNumber{})+len(plain)+MACSize)
	asdu[0] = byte(c)
	put48(asdu[1:], field)

	ad := asdu[:1]
	if serial != nil {
		asdu = append(asdu, serial[:]...)
		ad = append([]byte{byte(c)}, serial[:]...)
	}

	var n [SeqSize]byte
	put48(n[:], nonce)
	s := newStream(b, counter0(f, &n))

	var tag [16]byte
	if c&Confidential != 0 {
		tag = cbcMAC(b, block0(f, &n, len(plain)), ad, plain)

		start := len(asdu)
		asdu = append(asdu, plain...)
		s.xor(tag[:MACSize])
		s.xor(asdu[start:])
	} else {
		// Only authenticated: the APDU is part of the associated data and there is no payload.
		tag = cbcMAC(b, block0(f, &n, 0), append(ad[:len(ad):len(ad)], plain...), nil)
		s.xor(tag[:MACSize])
		asdu = append(asdu, plain...)
	}

	return append(asdu, tag[:MACSize]...)
}

// open checks the MAC and returns the plain APDU.
func (m Message) open(b cipher.Block, f Frame, nonce uint64, serial []byte) ([]byte, error) {
	if len(m.payload) > 0xFF {
		return nil, ErrInvalid
	}

	var n [SeqSize]byte
	put48(n[:], nonce)
	s := newStream(b, counter0(f, &n))

	tag := m.mac
	s.xor(tag[:])

	ad := append([]byte{byte(m.Control)}, serial...)
	plain := append([]byte(nil), m.payload...)

	var want [16]byte
	if m.Control&Confidential != 0 {
		s.xor(plain)
		want = cbcMAC(b, block0(f, &n, len(plain)), ad, plain)
	} else {
		want = cbcMAC(b, block0(f, &n, 0), append(ad, plain...), nil)
	}

	if subtle.ConstantTimeCompare(tag[:], want[:MACSize]) != 1 {
		return nil, ErrMAC
	}

	return plain, nil
}

// block0 returns the first block of the CBC-MAC. length is the length of the encrypted payload,
// 0 when the APDU is only authenticated.
func block0(f Frame, nonce *[SeqSize]byte, length int) *[16]byte {
	b := blockOf(f, nonce)

	b[11] = f.Format & 0x0F
	if f.Group {
		b[11] |= 0x80
	}

	b[12] = f.TPCI&0xFC | byte(apdu.ServiceSecure>>8)
	b[13] = byte(apdu.ServiceSecure & 0xFF)
	b[15] = byte(length)

	return b
}

// counter0 returns the first counter block of CTR mode, which encrypts the MAC.
func counter0(f Frame, nonce *[SeqSize]byte) *[16]byte {
	b := blockOf(f, nonce)
	b[14] = 1

	return b
}

func blockOf(f Frame, nonce *[SeqSize]byte) *[16]byte {
	var b [16]byte
	copy(b[:], nonce[:])
	b[6], b[7] = byte(f.Source>>8), byte(f.Source)
	b[8], b[9] = byte(f.Destination>>8), byte(f.Destination)

	return &b
}

// cbcMAC returns the MAC before encryption, of which Data Secure uses the first MACSize bytes:
// the last block of AES-CBC over b0 followed by the length of the associated data, the associated
// data and the payload, zero padded.
func cbcMAC(b cipher.Block, b0 *[16]byte, ad, payload []byte) [16]byte {
	m := chain{b: b}
	b.Encrypt(m.x[:], b0[:])

	m.write([]byte{byte(len(ad) >> 8), byte(len(ad))})
	m.write(ad)
	m.write(payload)

	if m.n > 0 {
		b.Encrypt(m.x[:], m.x[:])
	}

	return m.x
}

// chain is a running AES-CBC with a zero IV that keeps only its last block.
type chain struct {
	b cipher.Block
	x [16]byte
	n int // bytes of the current block
}

func (m *chain) write(p []byte) {
	for _, c := range p {
		m.x[m.n] ^= c
		m.n++

		if m.n == len(m.x) {
			m.b.Encrypt(m.x[:], m.x[:])
			m.n = 0
		}
	}
}

// stream is AES in CTR mode, counting up from counter block 0.
type stream struct {
	b   cipher.Block
	ctr [16]byte
	key [16]byte
	n   int // used bytes of key
}

func newStream(b cipher.Block, ctr0 *[16]byte) *stream {
	return &stream{b: b, ctr: *ctr0, n: 16}
}

func (s *stream) xor(p []byte) {
	for i := range p {
		if s.n == len(s.key) {
			s.b.Encrypt(s.key[:], s.ctr[:])
			s.n = 0

			for j := len(s.ctr) - 1; j >= 0; j-- {
				s.ctr[j]++
				if s.ctr[j] != 0 {
					break
				}
			}
		}

		p[i] ^= s.key[s.n]
		s.n++
	}
}

func get48(b []byte) uint64 {
	return uint64(b[0])<<40 | uint64(b[1])<<32 | uint64(b[2])<<24 | uint64(b[3])<<16 | uint64(b[4])<<8 | uint64(b[5])
}

func put48(b []byte, v uint64) {
	b[0], b[1], b[2], b[3], b[4], b[5] = byte(v>>40), byte(v>>32), byte(v>>24), byte(v>>16), byte(v>>8), byte(v)
}

// Secured returns the secure ASDU of application data that carries an A_SecureService.
func Secured(app *cemi.AppData) ([]byte, bool) {
	raw := apdu.RawFrom(app)
	return raw.Data, raw.Service == apdu.ServiceSecure
}

// Wrap returns application data that carries the secure ASDU, numbered like app.
func Wrap(app *cemi.AppData, asdu []byte) *cemi.AppData {
	secured := apdu.Raw{Service: apdu.ServiceSecure, Data: asdu}.AppData()
	secured.Numbered, secured.SeqNumber = app.Numbered, app.SeqNumber

	return secured
}

// Plain returns the APDU of application data in the form that Data Secure protects: the APCI in
// two bytes, followed by the data.
func Plain(app *cemi.AppData) []byte {
	plain := make([]byte, 2, 1+max(len(app.Data), 1))
	plain[0] = byte(app.Command>>2) & 3
	plain[1] = byte(app.Command&3) << 6

	if len(app.Data) > 0 {
		plain[1] |= app.Data[0] & 0x3F
		plain = append(plain, app.Data[1:]...)
	}

	return plain
}

// Unwrap returns application data that carries the plain APDU, numbered like app.
func Unwrap(app *cemi.AppData, plain []byte) (*cemi.AppData, error) {
	if len(plain) < 2 || plain[0]&^3 != 0 {
		return nil, ErrInvalid
	}

	data := append([]byte{plain[1] & 0x3F}, plain[2:]...)

	return &cemi.AppData{
		Numbered:  app.Numbered,
		SeqNumber: app.SeqNumber,
		Command:   cemi.APCI(plain[0]<<2 | plain[1]>>6),
		Data:      data,
	}, nil
}
