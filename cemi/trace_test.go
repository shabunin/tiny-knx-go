package cemi

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"testing"
)

// A transport-connection session captured from a BAOS module in link layer mode (see
// bb_digging_knx, "dobaosll_pub"): the client at 1.1.240 connects to 1.1.3, reads its device
// descriptor and acknowledges the response.
var tconnTrace = []struct {
	name     string
	frame    string
	code     MessageCode
	src, dst IndividualAddr
	tpdu     TransportUnit
}{
	{
		name:  "T_Connect.req",
		frame: "1100bc60000011030080",
		code:  LDataReqCode,
		src:   0,
		dst:   NewIndividualAddr3(1, 1, 3),
		tpdu:  &ControlData{Command: 0},
	},
	{
		name:  "T_Connect.con",
		frame: "2e00bc6011f011030080",
		code:  LDataConCode,
		src:   NewIndividualAddr3(1, 1, 240),
		dst:   NewIndividualAddr3(1, 1, 3),
		tpdu:  &ControlData{Command: 0},
	},
	{
		name:  "DeviceDescriptorRead.req",
		frame: "1100bc6000001103014300",
		code:  LDataReqCode,
		src:   0,
		dst:   NewIndividualAddr3(1, 1, 3),
		tpdu:  &AppData{Numbered: true, SeqNumber: 0, Command: MaskVersionRead, Data: []byte{0}},
	},
	{
		name:  "T_ACK.ind",
		frame: "2900b060110311f000c2",
		code:  LDataIndCode,
		src:   NewIndividualAddr3(1, 1, 3),
		dst:   NewIndividualAddr3(1, 1, 240),
		tpdu:  &ControlData{Numbered: true, SeqNumber: 0, Command: 2},
	},
	{
		name:  "DeviceDescriptorResponse.ind",
		frame: "2900bc60110311f00343400701",
		code:  LDataIndCode,
		src:   NewIndividualAddr3(1, 1, 3),
		dst:   NewIndividualAddr3(1, 1, 240),
		tpdu: &AppData{
			Numbered:  true,
			SeqNumber: 0,
			Command:   MaskVersionResponse,
			Data:      []byte{0x00, 0x07, 0x01},
		},
	},
	{
		name:  "T_ACK.req",
		frame: "1100bc600000110300c2",
		code:  LDataReqCode,
		src:   0,
		dst:   NewIndividualAddr3(1, 1, 3),
		tpdu:  &ControlData{Numbered: true, SeqNumber: 0, Command: 2},
	},
}

func ldataOf(msg Message) *LData {
	switch msg := msg.(type) {
	case *LDataReq:
		return &msg.LData
	case *LDataCon:
		return &msg.LData
	case *LDataInd:
		return &msg.LData
	}

	return nil
}

func equalTransportUnits(a, b TransportUnit) bool {
	switch a := a.(type) {
	case *ControlData:
		b, ok := b.(*ControlData)
		return ok && *a == *b

	case *AppData:
		b, ok := b.(*AppData)
		return ok && a.Numbered == b.Numbered && a.SeqNumber == b.SeqNumber &&
			a.Command == b.Command && bytes.Equal(a.Data, b.Data)
	}

	return false
}

func TestTransportConnectionTrace(t *testing.T) {
	for _, tc := range tconnTrace {
		t.Run(tc.name, func(t *testing.T) {
			frame, _ := hex.DecodeString(tc.frame)

			var msg Message
			n, err := Unpack(frame, &msg)
			if err != nil {
				t.Fatalf("Unpack failed: %v", err)
			}

			if n != uint(len(frame)) {
				t.Errorf("Unpack consumed %d bytes, expected %d", n, len(frame))
			}

			if msg.MessageCode() != tc.code {
				t.Errorf("Message code %v, expected %v", msg.MessageCode(), tc.code)
			}

			ldata := ldataOf(msg)
			if ldata == nil {
				t.Fatalf("Unexpected message type %T", msg)
			}

			if ldata.Control2.IsGroupAddr() {
				t.Error("Destination should be an individual address")
			}

			if hops := ldata.Control2.Hops(); hops != 6 {
				t.Errorf("Hop count %d, expected 6", hops)
			}

			if ldata.Source != tc.src || IndividualAddr(ldata.Destination) != tc.dst {
				t.Errorf("Addresses %v -> %v, expected %v -> %v",
					ldata.Source, IndividualAddr(ldata.Destination), tc.src, tc.dst)
			}

			if !equalTransportUnits(ldata.Data, tc.tpdu) {
				t.Errorf("TPDU %+v, expected %+v", ldata.Data, tc.tpdu)
			}

			packed := make([]byte, Size(msg))
			Pack(packed, msg)

			if !bytes.Equal(packed, frame) {
				t.Errorf("Packed % x, expected % x", packed, frame)
			}
		})
	}
}

func TestUnpackAppDataLength(t *testing.T) {
	// GroupValueWrite with 2 data bytes to 1/2/3.
	frame, _ := hex.DecodeString("2900bce011010a030300800c1a")

	var msg Message

	// Missing data bytes must be reported instead of being filled with zeros.
	if _, err := Unpack(frame[:len(frame)-2], &msg); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("Truncated frame: got error %v, expected %v", err, io.ErrUnexpectedEOF)
	}

	// Bytes after the TPDU are not part of the frame and must not make it invalid.
	n, err := Unpack(append(frame, 0xff), &msg)
	if err != nil {
		t.Fatalf("Frame with trailing byte: unexpected error %v", err)
	}

	if n != uint(len(frame)) {
		t.Errorf("Unpack consumed %d bytes, expected %d", n, len(frame))
	}
}

func TestControl1Request(t *testing.T) {
	// ETS sends 0xBC (see the trace above); system priority gives 0xB0.
	if got := Control1Request(PrioLow); got != 0xBC {
		t.Errorf("Control1Request(PrioLow) = %#x, expected 0xbc", got)
	}

	if got := Control1Request(PrioSystem); got != 0xB0 {
		t.Errorf("Control1Request(PrioSystem) = %#x, expected 0xb0", got)
	}
}
