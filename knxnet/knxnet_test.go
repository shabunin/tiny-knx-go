package knxnet

import (
	"bytes"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/shabunin/tiny-knx-go/cemi"
)

func mustHex(s string) []byte {
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		panic(err)
	}

	return b
}

// Packets from the KNXnet/IP specification (03_08_04 Tunnelling, examples) and from captures.
func TestKnownPackets(t *testing.T) {
	groupWrite := &cemi.LDataReq{LData: cemi.LData{
		Control1:    0xBC,
		Control2:    0xE0,
		Destination: 0x0A03,
		Data:        &cemi.AppData{Command: cemi.GroupValueWrite, Data: []byte{0}},
	}}

	packets := []struct {
		hex     string
		service Service
	}{
		{
			"06100205001a0801c0a8c80cc3b40801c0a8c80cc3b404040200",
			&ConnectRequest{
				Control: HPAI{ProtocolUDP, [4]byte{192, 168, 200, 12}, 50100},
				Data:    HPAI{ProtocolUDP, [4]byte{192, 168, 200, 12}, 50100},
				Type:    TunnelConnection,
				Layer:   TunnelLinkLayer,
			},
		},
		{
			"061002060014150008010a0000140e570404110a",
			&ConnectResponse{
				Channel: 0x15,
				Data:    HPAI{ProtocolUDP, [4]byte{10, 0, 0, 20}, 3671},
				Type:    TunnelConnection,
				Address: cemi.NewIndividualAddr3(1, 1, 10),
			},
		},
		{"0610020600081524", &ConnectResponse{Channel: 0x15, Status: StatusNoMoreConnections}},
		{"06100207001015000801000000000000", &ConnectionStateRequest{Channel: 0x15, Control: HPAI{Protocol: ProtocolUDP}}},
		{"0610020800081521", &ConnectionStateResponse{Channel: 0x15, Status: StatusConnectionID}},
		{"06100209001015000801000000000000", &DisconnectRequest{Channel: 0x15, Control: HPAI{Protocol: ProtocolUDP}}},
		{"0610020a00081500", &DisconnectResponse{Channel: 0x15}},
		{"061004200015041507001100bce000000a03010080", &TunnellingRequest{Channel: 0x15, Sequence: 7, Message: groupWrite}},
		{"06100421000a04150700", &TunnellingAck{Channel: 0x15, Sequence: 7}},
		{"06100531000a04000005", &RoutingLostMessage{Lost: 5}},
		{"06100532000c060000640000", &RoutingBusy{WaitTime: 100}},
		{"0610ffff0008abcd", &Unknown{ID: 0xffff, Body: []byte{0xab, 0xcd}}},
	}

	for _, p := range packets {
		data := mustHex(p.hex)

		if encoded := Encode(p.service); !bytes.Equal(encoded, data) {
			t.Errorf("Encode %T:\n got %x\nwant %x", p.service, encoded, data)
		}

		decoded, err := Decode(data)
		if err != nil {
			t.Errorf("Decode %s: %v", p.hex, err)
			continue
		}

		if !reflect.DeepEqual(decoded, p.service) {
			t.Errorf("Decode %s:\n got %+v\nwant %+v", p.hex, decoded, p.service)
		}
	}
}

func TestRoutingIndication(t *testing.T) {
	// From the bb_digging trace: 1.1.5 writes 1 to 1/1/1.
	data := mustHex("0610053000112900bce011050901010081")

	s, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}

	ind, ok := s.(*RoutingIndication).Message.(*cemi.LDataInd)
	if !ok || ind.Source != cemi.NewIndividualAddr3(1, 1, 5) || ind.Destination != 0x0901 {
		t.Fatalf("Decoded %+v", s)
	}

	if !bytes.Equal(Encode(s), data) {
		t.Errorf("Round trip gives %x", Encode(s))
	}
}

func TestSearchResponse(t *testing.T) {
	response := &SearchResponse{
		Control: HPAI{ProtocolUDP, [4]byte{192, 168, 1, 10}, Port},
		Device: DeviceInfo{
			Medium:         0x02,
			Status:         1,
			Address:        cemi.NewIndividualAddr3(1, 1, 0),
			SerialNumber:   [6]byte{0x00, 0xfa, 1, 2, 3, 4},
			MulticastGroup: RoutingGroup,
			MAC:            [6]byte{2, 0, 0, 0, 0, 1},
			Name:           "tiny-knx-go router",
			Families: []ServiceFamily{
				{FamilyCore, 1}, {FamilyDeviceManagement, 1}, {FamilyTunnelling, 1}, {FamilyRouting, 1},
			},
		},
	}

	data := Encode(response)
	if len(data) != 6+8+54+10 {
		t.Errorf("Packet length %d", len(data))
	}

	decoded, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(decoded, response) {
		t.Errorf("Round trip:\n got %+v\nwant %+v", decoded, response)
	}

	if d := decoded.(*SearchResponse).Device; !d.Supports(FamilyTunnelling) || d.Supports(0x07) {
		t.Error("Supports is wrong")
	}
}

func TestDecodeErrors(t *testing.T) {
	bad := []struct {
		hex string
		err error
	}{
		{"0610", ErrHeader},
		{"0510020800081500", ErrHeader},
		{"0620020800081500", ErrHeader},
		{"0610020800091500", ErrTruncated},
		{"06100208000815", ErrTruncated},
		{"061002080007150000", ErrTruncated},
		{"06100421000a05150700", ErrStructure},
		{"061002070010150009010000000000000", ErrStructure},
		{"06100420000a04150700", nil}, // a tunnelling request without cEMI fails in cemi.Unpack
	}

	for _, b := range bad {
		data, _ := hex.DecodeString(b.hex)

		_, err := Decode(data)
		if err == nil || (b.err != nil && !errors.Is(err, b.err)) {
			t.Errorf("Decode %s: got %v, want %v", b.hex, err, b.err)
		}
	}
}

func TestStatusError(t *testing.T) {
	var err error = StatusNoMoreConnections
	if err.Error() != "knxnet: no more connections" || Status(0x99).String() != "status 0x99" {
		t.Errorf("Got %q, %q", err, Status(0x99))
	}
}
