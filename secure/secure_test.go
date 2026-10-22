package secure

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/shabunin/tiny-knx-go/apdu"
	"github.com/shabunin/tiny-knx-go/cemi"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()

	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatal(err)
	}

	return b
}

func block(t *testing.T, key string) cipher.Block {
	t.Helper()

	b, err := aes.NewCipher(unhex(t, key))
	if err != nil {
		t.Fatal(err)
	}

	return b
}

// The tool key and addresses of the examples in AN158 Annex A, which calimero tests with.
const toolKey = "000102030405060708090a0b0c0d0e0f"

var (
	local  = cemi.NewIndividualAddr3(15, 15, 103)
	remote = cemi.NewIndividualAddr3(15, 15, 0)

	// The key of 0/4/0 in SecureTest.knxkeys of xknx, an ETS project, whose telegrams xknx tests
	// with.
	groupKey = "dfdf23a59fbb40404091d1c162087e8b"
	group    = uint16(cemi.NewGroupAddr3(0, 4, 0))
)

// propertyWrite is a property write of a key to the group key table (object 5, PID 0x35).
const propertyWrite = "03 d7 05 35 10 01 20 21 22 23 24 25 26 27 28 29 2a 2b 2c 2d 2e 2f"

func TestSeal(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		frame Frame
		c     Control
		seq   uint64
		plain string
		want  string
	}{
		{
			"property write with the tool key", toolKey,
			Frame{Source: local, Destination: uint16(remote)},
			ToolAccess | Confidential, 4, propertyWrite,
			"90 000000000004 6767242a2308ca76a11774214ee4cf5d94909f743d050d8fc168",
		},
		{
			"property response with the tool key", toolKey,
			Frame{Source: remote, Destination: uint16(local)},
			ToolAccess | Confidential, 3, "03 d6" + propertyWrite[5:],
			"90 000000000003 706f533105503557cb2b24f1dd341b60b7e017ecd6b06849a72b",
		},
		{
			"group value read", groupKey,
			Frame{Source: cemi.NewIndividualAddr3(5, 0, 1), Destination: group, Group: true},
			Confidential, 160170101607, "00 00",
			"10 00254ae1cb67 cd18 4afe5744",
		},
		{
			"group value write in an extended frame", groupKey,
			Frame{Source: cemi.NewIndividualAddr3(5, 0, 1), Destination: group, Group: true},
			Confidential, 160170101607, "00 80 ff 00 05",
			"10 00254ae1cb67 cd98e577b5 19be47bb",
		},
	}

	for _, test := range tests {
		b := block(t, test.key)
		want := unhex(t, test.want)

		if got := Seal(b, test.frame, test.c, test.seq, unhex(t, test.plain)); !bytes.Equal(got, want) {
			t.Errorf("%s: Seal = % x, want % x", test.name, got, want)
		}

		m, err := Parse(want)
		if err != nil {
			t.Errorf("%s: Parse: %v", test.name, err)
			continue
		}

		if m.Control != test.c || m.Seq != test.seq {
			t.Errorf("%s: Parse = %+v", test.name, m)
		}

		if plain, err := m.Open(b, test.frame); err != nil || !bytes.Equal(plain, unhex(t, test.plain)) {
			t.Errorf("%s: Open = % x, %v, want %s", test.name, plain, err, test.plain)
		}
	}
}

// TestReceived opens a telegram that xknx received from an ETS-configured device.
func TestReceived(t *testing.T) {
	var msg cemi.Message
	if _, err := cemi.Unpack(unhex(t, "29003ce0400904001103f110002446cfef4ac085e7092ab062b44d"), &msg); err != nil {
		t.Fatal(err)
	}

	ld := &msg.(*cemi.LDataInd).LData
	asdu, ok := Secured(ld.Data.(*cemi.AppData))
	if !ok {
		t.Fatal("not a secure APDU")
	}

	m, err := Parse(asdu)
	if err != nil {
		t.Fatal(err)
	}

	if m.Seq != 155806854986 {
		t.Errorf("Seq = %d", m.Seq)
	}

	f := FrameOf(ld)
	if f != (Frame{Source: cemi.NewIndividualAddr3(4, 0, 9), Destination: group, Group: true}) {
		t.Errorf("FrameOf = %+v", f)
	}

	plain, err := m.Open(block(t, groupKey), f)
	if err != nil {
		t.Fatal(err)
	}

	app, err := Unwrap(ld.Data.(*cemi.AppData), plain)
	if err != nil {
		t.Fatal(err)
	}

	got, err := apdu.Decode(app)
	if err != nil {
		t.Fatal(err)
	}

	if r, ok := got.(apdu.GroupValueResponse); !ok || !bytes.Equal(r.Data, []byte{0, 116, 41, 41}) {
		t.Errorf("APDU = %#v", got)
	}
}

func TestSync(t *testing.T) {
	b := block(t, toolKey)

	// Sync request and response in a connection (T_Data_Connected with sequence 0).
	req := Frame{Source: local, Destination: uint16(remote), TPCI: 0x40}
	res := Frame{Source: remote, Destination: uint16(local), TPCI: 0x40}

	want := unhex(t, "92 000000000001 000000000000 c1cf4506f09b d79fab55")
	if got := SealSyncRequest(b, req, ToolAccess, 1, apdu.SerialNumber{}, 3); !bytes.Equal(got, want) {
		t.Errorf("SealSyncRequest = % x, want % x", got, want)
	}

	m, err := Parse(want)
	if err != nil {
		t.Fatal(err)
	}

	if challenge, err := m.OpenSyncRequest(b, req); err != nil || challenge != 3 || m.Seq != 1 {
		t.Errorf("OpenSyncRequest = %d, %v; seq %d", challenge, err, m.Seq)
	}

	want = unhex(t, "93 aaaaaaaaaaa9 9c023ad25e146470693e638d 5b70cac4")
	if got := SealSyncResponse(b, res, ToolAccess, 0xaaaaaaaaaaaa, 3, 3, 4); !bytes.Equal(got, want) {
		t.Errorf("SealSyncResponse = % x, want % x", got, want)
	}

	if m, err = Parse(want); err != nil {
		t.Fatal(err)
	}

	if seq, remoteSeq, err := m.OpenSyncResponse(b, res, 3); err != nil || seq != 3 || remoteSeq != 4 {
		t.Errorf("OpenSyncResponse = %d, %d, %v", seq, remoteSeq, err)
	}

	if _, _, err := m.OpenSyncResponse(b, res, 4); err != ErrMAC {
		t.Errorf("OpenSyncResponse with another challenge = %v", err)
	}

	// A sync request to a device with its serial number authenticates the serial number.
	serial := apdu.SerialNumber{0x00, 0xfa, 1, 2, 3, 4}
	asdu := SealSyncRequest(b, req, ToolAccess, 7, serial, 0x123456789abc)

	if m, err = Parse(asdu); err != nil || m.Serial != serial {
		t.Fatalf("Parse = %+v, %v", m, err)
	}

	m.Serial[5]++
	if _, err := m.OpenSyncRequest(b, req); err != ErrMAC {
		t.Errorf("OpenSyncRequest with another serial number = %v", err)
	}
}

// TestTamper changes every bit of secure ASDUs, and the frames they are bound to.
func TestTamper(t *testing.T) {
	b := block(t, groupKey)
	f := Frame{Source: cemi.NewIndividualAddr3(1, 1, 1), Destination: 0x0901, Group: true}
	plain := []byte{0x00, 0x80, 0x0c, 0x1a}

	for _, c := range []Control{Confidential, 0} {
		asdu := Seal(b, f, c, 42, plain)

		m, err := Parse(asdu)
		if err != nil {
			t.Fatal(err)
		}

		if got, err := m.Open(b, f); err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("control %#x: Open = % x, %v", c, got, err)
		}

		if c == 0 && !bytes.Contains(asdu, plain) {
			t.Errorf("authenticated APDU is not plain text: % x", asdu)
		} else if c != 0 && bytes.Contains(asdu, plain) {
			t.Errorf("encrypted APDU is plain text: % x", asdu)
		}

		for i := range asdu {
			for bit := 0; bit < 8; bit++ {
				changed := append([]byte(nil), asdu...)
				changed[i] ^= 1 << bit

				m, err := Parse(changed)
				if err != nil {
					continue
				}

				if _, err := m.Open(b, f); err == nil {
					t.Errorf("control %#x: byte %d bit %d changed and Open succeeded", c, i, bit)
				}
			}
		}

		for _, other := range []Frame{
			{Source: f.Source + 1, Destination: f.Destination, Group: true},
			{Source: f.Source, Destination: f.Destination + 1, Group: true},
			{Source: f.Source, Destination: f.Destination},
			{Source: f.Source, Destination: f.Destination, Group: true, Format: 1},
			{Source: f.Source, Destination: f.Destination, Group: true, TPCI: 0x44},
		} {
			if _, err := m.Open(b, other); err != ErrMAC {
				t.Errorf("control %#x: Open in frame %+v = %v", c, other, err)
			}
		}
	}
}

func TestParse(t *testing.T) {
	for _, s := range []string{
		"",
		"10 000000000001 000000",      // too short for a MAC
		"20 000000000001 00 00000000", // unknown algorithm
		"11 000000000001 00 00000000", // unknown service
		"02 000000000001 000000000000 00 00000000", // sync request, not encrypted
		"12 000000000001 0000000000 00000000",      // sync request without a serial number
	} {
		if _, err := Parse(unhex(t, s)); err != ErrInvalid {
			t.Errorf("Parse(%s) = %v", s, err)
		}
	}
}

func TestPlain(t *testing.T) {
	for _, a := range []apdu.APDU{
		apdu.GroupValueRead{},
		apdu.GroupValueWrite{Data: []byte{1}},
		apdu.GroupValueWrite{Data: []byte{0, 0x0c, 0x1a}},
		apdu.PropertyValueWrite{PropertyRef: apdu.PropertyRef{Object: 5, PropertyID: 0x35, Count: 1, Start: 1}, Data: []byte{0x20, 0x21}},
	} {
		app := apdu.Encode(a)
		app.Numbered, app.SeqNumber = true, 5

		plain := Plain(app)
		back, err := Unwrap(app, plain)
		if err != nil {
			t.Fatal(err)
		}

		if got, _ := apdu.Decode(back); !back.Numbered || back.SeqNumber != 5 || !equalAPDU(got, a) {
			t.Errorf("Unwrap(Plain(%#v)) = %#v", a, got)
		}
	}

	if got := Plain(apdu.Encode(apdu.PropertyValueWrite{PropertyRef: apdu.PropertyRef{Object: 5, PropertyID: 0x35, Count: 1, Start: 1}, Data: []byte{0x20}})); !bytes.Equal(got, unhex(t, "03 d7 05 35 10 01 20")) {
		t.Errorf("Plain = % x", got)
	}

	secured := Wrap(&cemi.AppData{Numbered: true, SeqNumber: 3}, []byte{0x10, 1, 2})
	if asdu, ok := Secured(secured); !ok || !bytes.Equal(asdu, []byte{0x10, 1, 2}) || !secured.Numbered || secured.SeqNumber != 3 {
		t.Errorf("Secured(Wrap) = % x, %v, %+v", asdu, ok, secured)
	}
}

func equalAPDU(a, b apdu.APDU) bool {
	ra, rb := a.Raw(), b.Raw()
	return ra.Service == rb.Service && ra.Short == rb.Short && bytes.Equal(ra.Data, rb.Data)
}
