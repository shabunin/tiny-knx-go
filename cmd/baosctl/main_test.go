//go:build linux && !tinygo

package main

import (
	"fmt"
	"testing"
)

func TestParsePairs(t *testing.T) {
	tests := map[string]string{
		`1: true`:                            `[{1 true}]`,
		`1:true 2=21.5`:                      `[{1 true} {2 21.5}]`,
		`[lights: false, 2: -3]`:             `[{lights false} {2 -3}]`,
		`9: {day: 1, hour: 12} 10: "a b"`:    `[{9 {day: 1, hour: 12}} {10 "a b"}]`,
		`raw: [1, 2, 3], "my dp": 'x y'`:     `[{raw [1, 2, 3]} {my dp 'x y'}]`,
		`[1: {value: true, control: false}]`: `[{1 {value: true, control: false}}]`,
	}

	for in, want := range tests {
		got, err := parsePairs(in)
		if err != nil || fmt.Sprint(got) != want {
			t.Errorf("%s: %v, %v; want %s", in, got, err, want)
		}
	}

	for _, in := range []string{"", "1", "1:", "1: {day: 1", "1: 'x"} {
		if got, err := parsePairs(in); err == nil {
			t.Errorf("%q: %v", in, got)
		}
	}
}

func TestValues(t *testing.T) {
	for in, want := range map[string]string{
		`true`:                      `true`,
		`21.5`:                      `21.5`,
		`lights`:                    `lights`,
		`'x y'`:                     `x y`,
		`{day: 1, "hour": 12}`:      `map[day:1 hour:12]`,
		`{control: true, value: 0}`: `map[control:true value:0]`,
	} {
		got, err := jsonValue(in)
		if err != nil || fmt.Sprint(got) != want {
			t.Errorf("%s: %v, %v; want %s", in, got, err, want)
		}
	}

	for in, want := range map[string]string{`0x0c33`: "DDM=", `[12, 51]`: "DDM=", `"DDM="`: "DDM="} {
		if got, err := rawValue(in); err != nil || got != want {
			t.Errorf("raw %s: %q, %v", in, got, err)
		}
	}

	if got := fmt.Sprint(ids("[1, 2] lights")); got != "[1 2 lights]" {
		t.Errorf("ids %s", got)
	}

	if ids("*") != nil || ids("") != nil {
		t.Error("all datapoints")
	}
}
