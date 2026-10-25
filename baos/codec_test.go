package baos

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

// The messages in these tests were captured from a kBerry (BAOS 838, version 2) and a BAOS Module
// 820 (version 1).

func unhex(t *testing.T, s string) []byte {
	t.Helper()

	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatal(err)
	}

	return b
}

func TestRequests(t *testing.T) {
	for _, tt := range []struct {
		name string
		got  func() ([]byte, error)
		want string
	}{
		{"v2 GetServerItem", func() ([]byte, error) { return AppendRangeRequest(nil, Version2, GetServerItem, 1, 17) }, "f0 01 0001 0011"},
		{"v1 GetServerItem", func() ([]byte, error) { return AppendRangeRequest(nil, Version1, GetServerItem, 1, 15) }, "f0 01 01 0f"},
		{"v2 GetDatapointValue", func() ([]byte, error) { return AppendGetDatapointValue(nil, Version2, 1, 16, FilterAll) }, "f0 05 0001 0010 00"},
		{"v1 GetDatapointValue", func() ([]byte, error) { return AppendGetDatapointValue(nil, Version1, 1, 16, FilterValid) }, "f0 05 01 10"},
		{"v2 SetDatapointValue", func() ([]byte, error) {
			return AppendSetDatapointValue(nil, Version2, []SetValue{{ID: 1, Command: CommandSetAndSend, Data: []byte{0x0c, 0x1a}}, {ID: 2, Command: CommandRead}})
		}, "f0 06 0001 0002 0001 03 02 0c1a 0002 04 00"},
		{"v1 SetDatapointValue", func() ([]byte, error) {
			return AppendSetDatapointValue(nil, Version1, []SetValue{{ID: 3, Command: CommandSet, Data: []byte{1}}})
		}, "f0 06 03 01 03 11 01"},
		{"v2 SetServerItem", func() ([]byte, error) {
			return AppendSetServerItem(nil, Version2, []Item{{ID: ItemProgrammingMode, Data: []byte{1}}})
		}, "f0 02 000f 0001 000f 01 01"},
		{"SetParameterByte", func() ([]byte, error) { return AppendSetParameterByte(nil, 0, nil), nil }, "f0 08 0000 0000"},
	} {
		got, err := tt.got()
		if err != nil || !bytes.Equal(got, unhex(t, tt.want)) {
			t.Errorf("%s: % x, %v; want %s", tt.name, got, err, tt.want)
		}
	}

	if _, err := AppendRangeRequest(nil, Version1, GetDatapointValue, 256, 1); err != ErrIDRange {
		t.Errorf("version 1 ID 256: %v", err)
	}
}

func TestServerItemsV2(t *testing.T) {
	m, err := Parse(Version2, unhex(t, "f0 81 0001 0011 0001 06 0000c5080003 0002 01 10 0003 01 12 0004 02 00c5 0005 02 00c5 0006 02 0805 0007 01 10 0008 06 00c50101bab8 0009 04 00cbdb30 000a 01 01 000b 02 00fa 000c 02 0000 000d 01 01 000e 02 00fa 000f 01 00 0010 01 20 0011 01 01"))
	if err != nil {
		t.Fatal(err)
	}

	items, err := m.Items(Version2)
	if err != nil || len(items) != 17 {
		t.Fatalf("Items: %d, %v", len(items), err)
	}

	if items[7].ID != ItemSerialNumber || !bytes.Equal(items[7].Data, unhex(t, "00c50101bab8")) {
		t.Errorf("serial number: %+v", items[7])
	}

	if items[15].ID != ItemProtocolVersion || items[15].Data[0] != 0x20 {
		t.Errorf("protocol version: %+v", items[15])
	}
}

func TestServerItemsV1(t *testing.T) {
	m, err := Parse(Version1, unhex(t, "f0 81 01 0f 01 06 0000c5080201 02 01 30 03 01 21 04 02 00c5 05 02 00c5 06 02 0802 07 01 30 08 06 00c501004122 09 04 00cc3a59 0a 01 01 0b 02 00f0 0c 02 0000 0d 01 01 0e 02 00f0 0f 01 00"))
	if err != nil {
		t.Fatal(err)
	}

	items, err := m.Items(Version1)
	if err != nil || len(items) != 15 {
		t.Fatalf("Items: %d, %v", len(items), err)
	}

	if items[5].ID != ItemApplicationID || !bytes.Equal(items[5].Data, []byte{0x08, 0x02}) {
		t.Errorf("application: %+v", items[5])
	}

	if items[13].ID != ItemBufferSize || !bytes.Equal(items[13].Data, []byte{0x00, 0xf0}) {
		t.Errorf("buffer size: %+v", items[13])
	}
}

func TestDescriptions(t *testing.T) {
	m, _ := Parse(Version2, unhex(t, "f0 83 0001 000b 0001 08 d7 09 0002 00 57 01 0003 00 57 01 0004 00 57 01 0005 00 57 01 0006 00 d7 01 0007 00 d7 01 0008 00 d7 01 0009 00 d7 01 000a 00 57 01 000b 07 57 05"))

	d, err := m.Descriptions(Version2)
	if err != nil || len(d) != 11 {
		t.Fatalf("v2: %d, %v", len(d), err)
	}

	if d[0] != (Description{ID: 1, ValueType: Bytes2, Flags: 0xd7, DPT: 9}) {
		t.Errorf("v2 DP 1: %+v", d[0])
	}

	if d[10] != (Description{ID: 11, ValueType: Bytes1, Flags: 0x57, DPT: 5}) {
		t.Errorf("v2 DP 11: %+v", d[10])
	}

	if s := d[0].Flags.String(); s != "C-WTU-" {
		t.Errorf("flags 0xd7: %s", s)
	}

	if s := d[1].Flags.String(); s != "C-WT--" {
		t.Errorf("flags 0x57: %s", s)
	}

	// Version 1 numbers the descriptions from the start, without IDs.
	m, _ = Parse(Version1, unhex(t, "f0 88 01 03 08 df 09 0e db 00 0e db 00"))

	d, err = m.Descriptions(Version1)
	if err != nil || len(d) != 3 {
		t.Fatalf("v1: %d, %v", len(d), err)
	}

	if d[0] != (Description{ID: 1, ValueType: Bytes2, Flags: 0xdf, DPT: 9}) || d[2].ID != 3 || d[2].DPT != DPTDisabled {
		t.Errorf("v1: %+v", d)
	}

	if d[0].Flags.Priority() != 3 {
		t.Errorf("v1 priority: %d", d[0].Flags.Priority())
	}
}

func TestValues(t *testing.T) {
	m, _ := Parse(Version2, unhex(t, "f0 85 0001 0003 0001 00 02 0000 0002 18 01 01 0003 0b 01 00"))

	values, err := m.Values(Version2)
	if err != nil || len(values) != 3 {
		t.Fatalf("v2: %d, %v", len(values), err)
	}

	if values[0].State.Valid || !bytes.Equal(values[0].Data, []byte{0, 0}) {
		t.Errorf("v2 DP 1: %+v", values[0])
	}

	if s := values[1].State; !s.Valid || !s.Updated || s.Transmission != TransmissionIdle || values[1].Data[0] != 1 {
		t.Errorf("v2 DP 2: %+v", values[1])
	}

	if s := values[2].State; s.Valid || !s.Updated || s.Transmission != TransmissionRequest {
		t.Errorf("v2 DP 3: %+v", values[2])
	}

	// Version 1 packs state and length into one byte.
	m, _ = Parse(Version1, unhex(t, "f0 c1 01 01 01 a2 0c1a"))
	if m.Service != DatapointValueIndication {
		t.Fatalf("service %02x", m.Service)
	}

	values, err = m.Values(Version1)
	if err != nil || len(values) != 1 {
		t.Fatalf("v1: %d, %v", len(values), err)
	}

	if s := values[0].State; !s.Valid || !s.Updated || s.ReadRequest || s.Transmission != TransmissionInProgress || !bytes.Equal(values[0].Data, []byte{0x0c, 0x1a}) {
		t.Errorf("v1: %+v", values[0])
	}
}

func TestNegativeResponses(t *testing.T) {
	for _, tt := range []struct {
		v       Version
		message string
		want    error
	}{
		{Version2, "f0 84 0001 0000 05", ErrServiceNotSupported},
		{Version2, "f0 86 0001 0000 07", ErrBadID},
		{Version1, "f0 85 01 00 02", ErrNoElement},
		{Version1, "f0 87 00 00 06", ErrBadParameter},
		{Version2, "f0 82 000f 0000 00", nil}, // positive SetServerItem.Res
	} {
		m, err := Parse(tt.v, unhex(t, tt.message))
		if err != nil {
			t.Fatal(err)
		}

		if err := m.Err(); err != tt.want {
			t.Errorf("%s: %v, want %v", tt.message, err, tt.want)
		}
	}

	if _, err := Parse(Version2, unhex(t, "f0 81 0001")); err != ErrMalformed {
		t.Errorf("short message: %v", err)
	}

	m, _ := Parse(Version2, unhex(t, "f0 81 0001 0002 0001 06 0000"))
	if _, err := m.Items(Version2); err != ErrMalformed {
		t.Errorf("truncated items: %v", err)
	}
}

func TestStringsAndParameters(t *testing.T) {
	m, _ := Parse(Version2, unhex(t, "f0 84 0001 0002 0005 4c69676874 0000"))

	s, err := m.Strings(Version2, 0)
	if err != nil || len(s) != 2 || s[0] != "Light" || s[1] != "" {
		t.Errorf("v2 strings: %q, %v", s, err)
	}

	m, _ = Parse(Version1, unhex(t, "f0 84 01 02 4c6967 426c69"))

	s, err = m.Strings(Version1, 3)
	if err != nil || len(s) != 2 || s[0] != "Lig" || s[1] != "Bli" {
		t.Errorf("v1 strings: %q, %v", s, err)
	}

	m, _ = Parse(Version2, unhex(t, "f0 87 0001 0003 0a0b0c"))

	b, err := m.ParameterBytes()
	if err != nil || !bytes.Equal(b, []byte{10, 11, 12}) {
		t.Errorf("parameters: % x, %v", b, err)
	}
}
