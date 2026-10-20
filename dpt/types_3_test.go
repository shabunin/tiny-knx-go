package dpt

import (
	"bytes"
	"testing"
)

func TestDPT_3007(t *testing.T) {
	cases := []struct {
		value DPT_3007
		data  []byte
		text  string
	}{
		{DPT_3007{Increase: true, StepCode: 1}, []byte{0x09}, "increase by step 1"},
		{DPT_3007{Increase: false, StepCode: 7}, []byte{0x07}, "decrease by step 7"},
		{DPT_3007{Increase: true}, []byte{0x08}, "increase stop"},
	}

	for _, c := range cases {
		if data := c.value.Pack(); !bytes.Equal(data, c.data) {
			t.Errorf("%+v packed to % x, expected % x", c.value, data, c.data)
		}

		var dst DPT_3007
		if err := dst.Unpack(c.data); err != nil || dst != c.value || dst.String() != c.text {
			t.Errorf("% x unpacked to %+v (%s), %v", c.data, dst, dst, err)
		}
	}

	var dst DPT_3007
	if err := dst.Unpack([]byte{0, 1}); err != ErrInvalidLength {
		t.Errorf("Unpacking 2 bytes: %v", err)
	}
}

func TestDPT_3008(t *testing.T) {
	var dst DPT_3008
	if err := dst.Unpack(DPT_3008{Down: true, StepCode: 3}.Pack()); err != nil || !dst.Down || dst.StepCode != 3 {
		t.Errorf("Round trip: %+v, %v", dst, err)
	}

	if s := (DPT_3008{}).String(); s != "up stop" {
		t.Errorf("String %q", s)
	}
}
