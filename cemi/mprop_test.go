package cemi

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func TestMProp(t *testing.T) {
	testCases := []struct {
		name  string
		frame string
		msg   Message
	}{
		{
			// Captured from a kBerry: read the device descriptor (mask version) ...
			name:  "M_PropRead.req",
			frame: "fc000001531001",
			msg: &MPropReadReq{MProp{
				ObjectType: ObjectTypeDevice, Instance: 1, PropertyID: PIDDeviceDescriptor,
				Count: 1, Start: 1, Data: []byte{},
			}},
		},
		{
			// ... and the module answers with mask version 07B0.
			name:  "M_PropRead.con",
			frame: "fb00000153100107b0",
			msg: &MPropReadCon{MProp{
				ObjectType: ObjectTypeDevice, Instance: 1, PropertyID: PIDDeviceDescriptor,
				Count: 1, Start: 1, Data: []byte{0x07, 0xb0},
			}},
		},
		{
			name:  "M_PropWrite.req comm mode",
			frame: "f600080134100100",
			msg: &MPropWriteReq{MProp{
				ObjectType: ObjectTypeCEMIServer, Instance: 1, PropertyID: PIDCommMode,
				Count: 1, Start: 1, Data: []byte{CommModeDataLinkLayer},
			}},
		},
		{
			name:  "M_PropWrite.con negative",
			frame: "f50008013400010a",
			msg: &MPropWriteCon{MProp{
				ObjectType: ObjectTypeCEMIServer, Instance: 1, PropertyID: PIDCommMode,
				Count: 0, Start: 1, Data: []byte{0x0a},
			}},
		},
		{
			name:  "M_Reset.ind",
			frame: "f0",
			msg:   &MResetInd{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			frame, _ := hex.DecodeString(tc.frame)

			packed := make([]byte, Size(tc.msg))
			Pack(packed, tc.msg)

			if !bytes.Equal(packed, frame) {
				t.Errorf("Packed % x, expected % x", packed, frame)
			}

			var msg Message
			n, err := Unpack(frame, &msg)
			if err != nil {
				t.Fatalf("Unpack failed: %v", err)
			}

			if n != uint(len(frame)) {
				t.Errorf("Unpack consumed %d bytes, expected %d", n, len(frame))
			}

			if msg.MessageCode() != tc.msg.MessageCode() {
				t.Fatalf("Message code %v, expected %v", msg.MessageCode(), tc.msg.MessageCode())
			}

			repacked := make([]byte, Size(msg))
			Pack(repacked, msg)

			if !bytes.Equal(repacked, frame) {
				t.Errorf("Repacked % x, expected % x", repacked, frame)
			}
		})
	}
}

func TestMPropFailed(t *testing.T) {
	frame, _ := hex.DecodeString("f50008013400010a")

	var msg Message
	if _, err := Unpack(frame, &msg); err != nil {
		t.Fatalf("Unpack failed: %v", err)
	}

	con := msg.(*MPropWriteCon)
	if !con.Failed() || con.Data[0] != 0x0a {
		t.Errorf("Expected negative confirmation with error 0x0a, got %+v", con.MProp)
	}
}

func TestLRawIndCode(t *testing.T) {
	if code := (LRawInd{}).MessageCode(); code != LRawIndCode {
		t.Errorf("LRawInd message code %v, expected %v", code, LRawIndCode)
	}
}
