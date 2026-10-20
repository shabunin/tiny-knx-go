package tpuart

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/shabunin/tiny-knx-go/cemi"
)

func decodeHex(t *testing.T, s string) []byte {
	t.Helper()

	data, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}

	return data
}

func packLData(ldata *cemi.LData) []byte {
	buf := make([]byte, ldata.Size())
	ldata.Pack(buf)

	return buf
}

// Frames from a kBerry trace (cemi/trace_test.go) and the TP1 frames they correspond to.
func TestFrameFromTrace(t *testing.T) {
	cases := []struct {
		name, cemi, tp string
	}{
		{"T_ACK", "2900b060110311f000c2", "b0110311f060c21e"},
		{"DeviceDescriptorResponse", "2900bc60110311f00343400701", "bc110311f06343400701d6"},
		{"GroupValueWrite", "2900bce011010a030300800c1a", "bc11010a03e300800c1a2f"},
	}

	for _, tc := range cases {
		var msg cemi.Message
		if _, err := cemi.Unpack(decodeHex(t, tc.cemi), &msg); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}

		ldata := msg.(*cemi.LDataInd).LData

		frame, err := AppendFrame(nil, &ldata)
		if err != nil || !bytes.Equal(frame, decodeHex(t, tc.tp)) {
			t.Errorf("%s: AppendFrame = %x, %v; expected %s", tc.name, frame, err, tc.tp)
		}

		parsed, err := ParseFrame(frame)
		if err != nil {
			t.Fatalf("%s: ParseFrame: %v", tc.name, err)
		}

		if got, want := packLData(&parsed), decodeHex(t, tc.cemi)[1:]; !bytes.Equal(got, want) {
			t.Errorf("%s: round trip gave cEMI %x, expected %x", tc.name, got, want)
		}
	}
}

func TestChecksum(t *testing.T) {
	frame := decodeHex(t, "bc11050901e100813f")

	if cs := Checksum(frame[:len(frame)-1]); cs != 0x3f {
		t.Errorf("Checksum = %#x, expected 0x3f", cs)
	}

	// All bytes of a valid frame XOR to 0xFF.
	if cs := Checksum(frame); cs != 0 {
		t.Errorf("Checksum over a whole frame = %#x, expected 0", cs)
	}
}

func groupWriteLData(dest uint16, data ...byte) cemi.LData {
	return cemi.LData{
		Control1:    cemi.Control1Request(cemi.PrioLow),
		Control2:    cemi.Control2GroupAddr | cemi.Control2Hops(6),
		Source:      cemi.NewIndividualAddr3(1, 1, 5),
		Destination: dest,
		Data:        &cemi.AppData{Command: cemi.GroupValueWrite, Data: data},
	}
}

func TestExtendedFrame(t *testing.T) {
	data := make([]byte, 40)
	for i := range data {
		data[i] = byte(i)
	}

	ldata := groupWriteLData(0x0901, data...)

	frame, err := AppendFrame(nil, &ldata)
	if err != nil {
		t.Fatal(err)
	}

	// ctrl, ctrlE, source, destination, length, TPCI, 40 bytes APCI/data, checksum
	if len(frame) != 1+1+2+2+1+1+40+1 || frame[0] != 0x3c || frame[1] != 0xe0 || frame[6] != 40 {
		t.Fatalf("Extended frame % x", frame)
	}

	parsed, err := ParseFrame(frame)
	if err != nil {
		t.Fatal(err)
	}

	app := parsed.Data.(*cemi.AppData)
	if parsed.Control1&cemi.Control1StdFrame != 0 || parsed.Control2 != ldata.Control2 ||
		parsed.Source != ldata.Source || parsed.Destination != 0x0901 ||
		app.Command != cemi.GroupValueWrite || !bytes.Equal(app.Data, data) {
		t.Errorf("Parsed %+v, data % x", parsed, app.Data)
	}

	// A short frame is extended too if the cEMI control field asks for it.
	short := groupWriteLData(0x0901, 1)
	short.Control1 &^= cemi.Control1StdFrame

	frame, _ = AppendFrame(nil, &short)
	if isStandard(frame[0]) || len(frame) != 10 {
		t.Errorf("Short extended frame % x", frame)
	}

	long := groupWriteLData(0x0901, make([]byte, 255)...)
	if _, err := AppendFrame(nil, &long); !errors.Is(err, ErrFrameTooLong) {
		t.Errorf("Frame with 255 bytes: %v", err)
	}
}

func TestControlFrame(t *testing.T) {
	ldata := cemi.LData{
		Control1:    cemi.Control1Request(cemi.PrioSystem),
		Control2:    cemi.Control2Hops(6),
		Source:      cemi.NewIndividualAddr3(1, 1, 240),
		Destination: uint16(cemi.NewIndividualAddr3(1, 1, 3)),
		Data:        &cemi.ControlData{Command: 0}, // T_Connect
	}

	frame, err := AppendFrame(nil, &ldata)
	if err != nil || !bytes.Equal(frame[:7], decodeHex(t, "b011f0110360"+"80")) || len(frame) != 8 {
		t.Fatalf("T_Connect frame % x, %v", frame, err)
	}

	parsed, err := ParseFrame(frame)
	if err != nil {
		t.Fatal(err)
	}

	if control, ok := parsed.Data.(*cemi.ControlData); !ok || control.Command != 0 || control.Numbered {
		t.Errorf("Parsed TPDU %+v", parsed.Data)
	}
}

func TestAppendFrameNormalisesControl(t *testing.T) {
	ldata := groupWriteLData(0x0901, 1)

	// cEMI bits that have no meaning on TP1: repeat allowed (0), system broadcast (0),
	// acknowledgement request and confirmation.
	ldata.Control1 = cemi.Control1StdFrame | cemi.Control1Prio(cemi.PrioLow) | cemi.Control1WantAck |
		cemi.Control1HasError

	frame, _ := AppendFrame(nil, &ldata)
	if frame[0] != 0xbc {
		t.Errorf("Control byte %#x, expected 0xbc", frame[0])
	}
}

func TestParseFrameErrors(t *testing.T) {
	frame := decodeHex(t, "bc11050901e100813f")

	broken := append([]byte(nil), frame...)
	broken[7] ^= 1

	cases := []struct {
		name  string
		frame []byte
		err   error
	}{
		{"checksum", broken, ErrChecksum},
		{"truncated", frame[:len(frame)-1], ErrInvalidFrame},
		{"too long", append(append([]byte(nil), frame...), 0), ErrInvalidFrame},
		{"header only", frame[:4], ErrInvalidFrame},
	}

	for _, tc := range cases {
		if _, err := ParseFrame(tc.frame); !errors.Is(err, tc.err) {
			t.Errorf("%s: got %v, expected %v", tc.name, err, tc.err)
		}
	}
}

func TestAppendSendRequest(t *testing.T) {
	got := appendSendRequest(nil, []byte{0xbc, 0x11, 0x3f})
	if want := []byte{0x80, 0xbc, 0x81, 0x11, 0x42, 0x3f}; !bytes.Equal(got, want) {
		t.Errorf("Request % x, expected % x", got, want)
	}

	// Positions beyond 63 need an offset.
	frame := make([]byte, 66)
	got = appendSendRequest(nil, frame)

	if want := []byte{0x80 | 63, 0, 0x09, 0x80, 0, 0x09, 0x40 | 1, 0}; !bytes.Equal(got[63*2:], want) {
		t.Errorf("Request ends with % x, expected % x", got[63*2:], want)
	}
}

func feedAll(r *receiver, data []byte, now time.Time) []rxKind {
	var kinds []rxKind
	for _, b := range data {
		if kind := r.feed(b, now); kind != rxNothing {
			kinds = append(kinds, kind)
		}
	}

	return kinds
}

func equalKinds(a, b []rxKind) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

func TestReceiver(t *testing.T) {
	r := newReceiver(50*time.Millisecond, 4*time.Millisecond)
	now := time.Now()
	frame := decodeHex(t, "bc11050901e100813f")

	// Reset indication, state, a frame, a positive confirmation.
	stream := append([]byte{ResetInd, 0x47}, frame...)
	stream = append(stream, 0x8b)

	got := feedAll(r, stream, now)
	want := []rxKind{rxReset, rxStateInd, rxFrameStart, rxHeader, rxFrame, rxConfirm}

	if !equalKinds(got, want) || r.value == 0 {
		t.Fatalf("Got %v, expected %v", got, want)
	}

	if feedAll(r, []byte{0x0b}, now); r.value != 0 {
		t.Error("Negative confirmation reported as positive")
	}

	// A bad checksum makes the receiver ignore everything until the line is quiet.
	broken := append([]byte(nil), frame...)
	broken[8] ^= 0xff

	got = feedAll(r, append(broken, frame...), now)
	if want := []rxKind{rxFrameStart, rxHeader, rxInvalidFrame}; !equalKinds(got, want) {
		t.Errorf("Broken frame: got %v, expected %v", got, want)
	}

	got = feedAll(r, frame, now.Add(10*time.Millisecond))
	if want := []rxKind{rxFrameStart, rxHeader, rxFrame}; !equalKinds(got, want) {
		t.Errorf("After a pause: got %v, expected %v", got, want)
	}

	// A frame that stops halfway is broken off by the next byte after a long pause.
	now = now.Add(time.Second)
	feedAll(r, frame[:5], now)

	if kind := r.feed(ResetInd, now.Add(100*time.Millisecond)); kind != rxReset {
		t.Errorf("Byte after an interrupted frame: got %v, expected %v", kind, rxReset)
	}

	feedAll(r, frame[:5], now)

	got = feedAll(r, frame, now.Add(100*time.Millisecond))
	if want := []rxKind{rxFrameStart, rxHeader, rxFrame}; !equalKinds(got, want) {
		t.Errorf("Interrupted frame: got %v, expected %v", got, want)
	}

	// An unknown byte also makes the receiver wait for a pause. (0xFF would be a U_State.ind.)
	now = now.Add(time.Second)

	got = feedAll(r, append([]byte{0x0a}, frame...), now)
	if want := []rxKind{rxUnknown}; !equalKinds(got, want) {
		t.Errorf("Unknown byte: got %v, expected %v", got, want)
	}
}
