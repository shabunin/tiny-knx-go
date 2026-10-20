package dpt

import (
	"bytes"
	"testing"
)

func TestDPT_20102(t *testing.T) {
	knxValue := []byte{0, 4}
	dptValue := DPT_20102(4)

	var tmpDPT DPT_20102
	if err := tmpDPT.Unpack(knxValue); err != nil {
		t.Fatalf("Unpack failed: %v", err)
	}
	if tmpDPT != dptValue {
		t.Errorf("Unpacked %v, expected %v", tmpDPT, dptValue)
	}

	if packed := dptValue.Pack(); !bytes.Equal(packed, knxValue) {
		t.Errorf("Packed %v, expected %v", packed, knxValue)
	}

	if str := dptValue.String(); str != "Building Protection" {
		t.Errorf("String() = %q, expected %q", str, "Building Protection")
	}
}

func TestDPT_20105(t *testing.T) {
	knxValue := []byte{0, 9}
	dptValue := DPT_20105(9)

	tmpDPT, ok := Produce("20.105")
	if !ok {
		t.Fatal("Produce failed")
	}
	if err := tmpDPT.Unpack(knxValue); err != nil {
		t.Fatalf("Unpack failed: %v", err)
	}
	if got, ok := tmpDPT.(*DPT_20105); !ok || *got != dptValue {
		t.Errorf("Unpacked %v, expected %v", tmpDPT, dptValue)
	}

	if packed := dptValue.Pack(); !bytes.Equal(packed, knxValue) {
		t.Errorf("Packed %v, expected %v", packed, knxValue)
	}

	if str := dptValue.String(); str != "Fan only" {
		t.Errorf("String() = %q, expected %q", str, "Fan only")
	}
}
