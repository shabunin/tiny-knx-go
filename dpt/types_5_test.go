package dpt

import (
	"bytes"
	"math"
	"math/rand"
	"testing"
)

// Test DPT 5.001 (Scaling) with values within range
func TestDPT_5001(t *testing.T) {
	var buf []byte
	var src, dst DPT_5001

	// Calculate the quantization error we expect
	const Q = float32(100) / 255

	for i := 1; i <= 10; i++ {
		value := rand.Float32()

		// Scale the random number to the given range
		value *= 100

		// Pack and unpack to test value
		src = DPT_5001(value)
		if abs(float32(src)-value) > epsilon {
			t.Errorf("Assignment of value \"%v\" failed for source of type DPT_5001! Has value \"%s\".", value, src)
		}
		buf = src.Pack()
		dst.Unpack(buf)
		if math.IsNaN(float64(dst)) {
			t.Errorf("Value \"%s\" is not a valid number! Original value was \"%v\".", dst, value)
		}
		if abs(float32(dst)-value) > (Q + epsilon) {
			t.Errorf("Value \"%s\" after pack/unpack above quantization noise! Original value was \"%v\", noise is \"%f\"", dst, value, Q)
		}
	}
}

// Test DPT 5.003 (Angle) with values within range
func TestDPT_5003(t *testing.T) {
	var buf []byte
	var src, dst DPT_5003

	// Calculate the quantization error we expect
	const Q = float32(360) / 255

	for i := 1; i <= 10; i++ {
		value := rand.Float32()

		// Scale the random number to the given range
		value *= 360

		// Pack and unpack to test value
		src = DPT_5003(value)
		if abs(float32(src)-value) > epsilon {
			t.Errorf("Assignment of value \"%v\" failed for source of type DPT_5003! Has value \"%s\".", value, src)
		}
		buf = src.Pack()
		dst.Unpack(buf)
		if math.IsNaN(float64(dst)) {
			t.Errorf("Value \"%s\" is not a valid number! Original value was \"%v\".", dst, value)
		}
		if abs(float32(dst)-value) > (Q + epsilon) {
			t.Errorf("Value \"%s\" after pack/unpack above quantization noise! Original value was \"%v\", noise is \"%f\"", dst, value, Q)
		}
	}
}

func TestDPT_5005(t *testing.T) {
	knxValue := []byte{0, 42}
	dptValue := DPT_5005(42)

	var tmpDPT DPT_5005
	if err := tmpDPT.Unpack(knxValue); err != nil {
		t.Fatalf("Unpack failed: %v", err)
	}
	if tmpDPT != dptValue {
		t.Errorf("Unpacked %v, expected %v", tmpDPT, dptValue)
	}

	if packed := dptValue.Pack(); !bytes.Equal(packed, knxValue) {
		t.Errorf("Packed %v, expected %v", packed, knxValue)
	}

	if str := dptValue.String(); str != "42" {
		t.Errorf("String() = %q, expected %q", str, "42")
	}
}
