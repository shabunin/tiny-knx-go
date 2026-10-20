package dpt

import (
	"bytes"
	"testing"
)

func TestDPT_6010(t *testing.T) {
	knxValue := []byte{0, 42}
	dptValue := DPT_6010(42)

	var tmpDPT DPT_6010
	if err := tmpDPT.Unpack(knxValue); err != nil {
		t.Fatalf("Unpack failed: %v", err)
	}
	if tmpDPT != dptValue {
		t.Errorf("Unpacked %v, expected %v", tmpDPT, dptValue)
	}

	if packed := dptValue.Pack(); !bytes.Equal(packed, knxValue) {
		t.Errorf("Packed %v, expected %v", packed, knxValue)
	}

	if str := dptValue.String(); str != "42 counter pulses" {
		t.Errorf("String() = %q, expected %q", str, "42 counter pulses")
	}
}
