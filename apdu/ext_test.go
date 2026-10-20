package apdu

import (
	"bytes"
	"testing"
)

func TestMemoryExt(t *testing.T) {
	cases := []struct {
		apdu    APDU
		encoded []byte
	}{
		{MemoryExtWrite{Address: 0x012345, Data: []byte{0xaa, 0xbb}}, []byte{0x01, 0xfb, 0x02, 0x01, 0x23, 0x45, 0xaa, 0xbb}},
		{MemoryExtWriteResponse{Code: ReturnSuccess, Address: 0x012345}, []byte{0x01, 0xfc, 0x00, 0x01, 0x23, 0x45}},
		{MemoryExtRead{Count: 4, Address: 0x4000}, []byte{0x01, 0xfd, 0x04, 0x00, 0x40, 0x00}},
		{MemoryExtReadResponse{Code: ReturnSuccess, Address: 0x4000, Data: []byte{1, 2}}, []byte{0x01, 0xfe, 0x00, 0x00, 0x40, 0x00, 1, 2}},
	}

	for _, c := range cases {
		app := Encode(c.apdu)

		encoded := append([]byte{byte(app.Command >> 2)}, app.Data...)
		encoded[1] |= byte(app.Command&3) << 6

		if !bytes.Equal(encoded, c.encoded) {
			t.Errorf("%T encoded as % x, expected % x", c.apdu, encoded, c.encoded)
		}

		decoded, err := Decode(app)
		if err != nil || !bytes.Equal(append([]byte(nil), Encode(decoded).Data...), app.Data) {
			t.Errorf("%T decoded as %+v, %v", c.apdu, decoded, err)
		}
	}

	if _, err := DecodeRaw(Raw{Service: ServiceMemoryExtWrite, Data: []byte{4, 0, 0, 0, 1}}); err != ErrTooShort {
		t.Errorf("Short MemoryExtWrite: %v", err)
	}
}
