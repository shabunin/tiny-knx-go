// Package gateway holds the parts of knxvbus and knxgw that connect lines and serve them: opening
// the media that reach a real line, bridging them to a virtual line, hosting the module of a serial
// interface as a device, and serving the ObjectServer of a BAOS module over TCP.
package gateway

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/shabunin/tiny-knx-go/cemi"
)

// A Spec names a medium and its options, like a URL: scheme:target?key=value&key=value, e.g.
// ft12:/dev/ttyAMA0?baud=19200 or knxip:wlan0?port=3671. A spec without a colon is a scheme alone,
// e.g. monitor.
type Spec struct {
	Scheme string
	Target string
	Params url.Values

	text string
	used map[string]bool
}

// ParseSpec parses a spec.
func ParseSpec(s string) (*Spec, error) {
	scheme, rest, _ := strings.Cut(s, ":")
	target, query, _ := strings.Cut(rest, "?")

	params, err := url.ParseQuery(query)
	if err != nil {
		return nil, fmt.Errorf("%q: %v", s, err)
	}

	if scheme == "" {
		return nil, fmt.Errorf("%q: no medium", s)
	}

	return &Spec{Scheme: scheme, Target: target, Params: params, text: s, used: map[string]bool{}}, nil
}

func (s *Spec) String() string { return s.text }

// Has reports whether the option is given, even without a value (e.g. ?device).
func (s *Spec) Has(key string) bool {
	s.used[key] = true
	_, ok := s.Params[key]

	return ok
}

// Get returns an option, or def if it is not given.
func (s *Spec) Get(key, def string) string {
	if s.Has(key) {
		return s.Params.Get(key)
	}

	return def
}

// Int returns a numeric option.
func (s *Spec) Int(key string, def int) (int, error) {
	if !s.Has(key) {
		return def, nil
	}

	n, err := strconv.Atoi(s.Params.Get(key))
	if err != nil {
		return 0, fmt.Errorf("%s: option %s: %v", s, key, err)
	}

	return n, nil
}

// Duration returns an option that is a duration, e.g. 500ms.
func (s *Spec) Duration(key string, def time.Duration) (time.Duration, error) {
	if !s.Has(key) {
		return def, nil
	}

	d, err := time.ParseDuration(s.Params.Get(key))
	if err != nil {
		return 0, fmt.Errorf("%s: option %s: %v", s, key, err)
	}

	return d, nil
}

// Address returns an option that is an individual address.
func (s *Spec) Address(key string, def cemi.IndividualAddr) (cemi.IndividualAddr, error) {
	if !s.Has(key) || s.Params.Get(key) == "" {
		return def, nil
	}

	addr, err := ParseAddress(s.Params.Get(key))
	if err != nil {
		return 0, fmt.Errorf("%s: option %s: %v", s, key, err)
	}

	return addr, nil
}

// Addresses returns an option that is a comma-separated list of individual addresses.
func (s *Spec) Addresses(key string) ([]cemi.IndividualAddr, error) {
	var list []cemi.IndividualAddr

	for _, a := range strings.Split(s.Get(key, ""), ",") {
		if a = strings.TrimSpace(a); a == "" {
			continue
		}

		addr, err := ParseAddress(a)
		if err != nil {
			return nil, fmt.Errorf("%s: option %s: %v", s, key, err)
		}

		list = append(list, addr)
	}

	return list, nil
}

// Unused returns an error for options that were not asked for, which are likely misspelled.
func (s *Spec) Unused() error {
	for key := range s.Params {
		if !s.used[key] {
			return fmt.Errorf("%s: unknown option %q", s, key)
		}
	}

	return nil
}

// ParseAddress parses an individual address, e.g. 1.1.250.
func ParseAddress(s string) (cemi.IndividualAddr, error) {
	return cemi.NewIndividualAddrString(strings.TrimSpace(s))
}
