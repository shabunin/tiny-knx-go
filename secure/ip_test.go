package secure

import (
	"bytes"
	"crypto/aes"
	"testing"
)

// The examples of AN159, which xknx tests with.

func TestDeriveKeys(t *testing.T) {
	if got, want := DeviceAuthKey("trustme"), unhex(t, "e1 58 e4 01 20 47 bd 6c c4 1a af bc 5c 04 c1 fc"); !bytes.Equal(got[:], want) {
		t.Errorf("DeviceAuthKey = % x, want % x", got, want)
	}

	if got, want := UserPasswordKey("secret"), unhex(t, "03 fc ed b6 66 60 25 1e c8 1a 1a 71 69 01 69 6a"); !bytes.Equal(got[:], want) {
		t.Errorf("UserPasswordKey = % x, want % x", got, want)
	}
}

func TestHandshakeMAC(t *testing.T) {
	key := DeviceAuthKey("trustme")
	b, _ := aes.NewCipher(key[:])

	// SESSION_RESPONSE with session 1, and the XOR of the public keys.
	ad := unhex(t, "06 10 09 52 00 38 00 01 b7 52 be 24 64 59 26 0f 6b 0c 48 01 fb d5 a6 75 99 f8 3b 40 57 b3 ef 1e 79 e4 69 ac 17 23 4e 15")

	var want [16]byte
	copy(want[:], unhex(t, "a9 22 50 5a aa 43 61 63 57 0b d5 49 4c 2d f2 a3"))

	if got := HandshakeMAC(b, ad); got != want {
		t.Errorf("HandshakeMAC = % x, want % x", got, want)
	}

	if !CheckHandshakeMAC(b, ad, want) {
		t.Error("CheckHandshakeMAC failed")
	}

	ad[10]++
	if CheckHandshakeMAC(b, ad, want) {
		t.Error("CheckHandshakeMAC accepted a changed packet")
	}
}

func TestSealPacket(t *testing.T) {
	b, _ := aes.NewCipher(unhex(t, toolKey))

	// A ROUTING_INDICATION in a SECURE_WRAPPER of secure routing.
	n := Nonce{Seq: 0xc0c1c2c3c4c5, Serial: [6]byte{0x00, 0xfa, 0x12, 0x34, 0x56, 0x78}, Tag: 0xaffe}
	ad := unhex(t, "06 10 09 50 00 37 00 00")
	packet := unhex(t, "06 10 05 30 00 11 29 00 bc d0 11 59 0a de 01 00 81")

	data, mac := SealPacket(b, n, ad, packet)
	if want := unhex(t, "b7 ee 7e 8a 1c 2f 7b ba be c7 75 fd 6e 10 d0 bc 4b"); !bytes.Equal(data, want) {
		t.Errorf("SealPacket data = % x, want % x", data, want)
	}

	if want := unhex(t, "72 12 a0 3a aa e4 9d a8 56 89 77 4c 1d 2b 4d a4"); !bytes.Equal(mac[:], want) {
		t.Errorf("SealPacket MAC = % x, want % x", mac, want)
	}

	if got, err := OpenPacket(b, n, ad, data, mac); err != nil || !bytes.Equal(got, packet) {
		t.Errorf("OpenPacket = % x, %v", got, err)
	}

	n.Seq++
	if _, err := OpenPacket(b, n, ad, data, mac); err != ErrMAC {
		t.Errorf("OpenPacket with another sequence number = %v", err)
	}
}
