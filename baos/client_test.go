package baos

import (
	"bytes"
	"testing"
	"time"
)

// fakeServer is an ObjectServer of either version behind a Transport. In link layer mode it
// ignores ObjectServer requests until it is switched to ObjectServer mode.
type fakeServer struct {
	v         Version
	linkMode  bool
	buffer    uint16 // reported in ItemBufferSize
	capacity  int    // real limit of a response, if smaller
	maxDP     uint16
	values    map[uint16][]byte // datapoints with a value
	types     map[uint16]ValueType
	requests  int
	inbound   chan []byte
	lastWrite []SetValue
}

func newFakeServer(v Version) *fakeServer {
	return &fakeServer{
		v:       v,
		buffer:  250,
		maxDP:   250,
		values:  map[uint16][]byte{1: {0x0c, 0x1a}, 3: {1}},
		types:   map[uint16]ValueType{1: Bytes2, 2: Bits1, 3: Bits1},
		inbound: make(chan []byte, 16),
	}
}

func (s *fakeServer) Inbound() <-chan []byte { return s.inbound }

func (s *fakeServer) Send(payload []byte) error {
	switch {
	case bytes.Equal(payload, cemiObjectServerMode):
		if s.v == Version2 {
			s.linkMode = false
		}

		return nil

	case bytes.Equal(payload, emiNormalMode):
		if s.v == Version1 {
			s.linkMode = false
		}

		return nil
	}

	if s.linkMode || len(payload) < 2 || payload[0] != MainService {
		return nil
	}

	// A server ignores requests of the other version, which have a different length.
	if s.v > Version2 || len(payload) < s.v.headerSize() {
		return nil
	}

	if payload[1] != SetDatapointValue && payload[1] != SetServerItem {
		want := s.v.headerSize()
		if s.v == Version2 && payload[1] == GetDatapointValue {
			want++
		}

		if len(payload) != want {
			return nil
		}
	}

	s.requests++

	m, _ := Parse(s.v, payload)
	res := []byte{MainService, m.Service | ResponseFlag}
	res = appendWord(res, s.v, m.Start)

	negative := func(code ErrorCode, at uint16) []byte {
		res = appendWord(res[:2], s.v, at)
		return append(appendWord(res, s.v, 0), byte(code))
	}

	switch m.Service {
	case GetServerItem:
		var n uint16
		var body []byte

		for id := m.Start; id < m.Start+m.Count; id++ {
			var data []byte

			switch id {
			case ItemFirmwareVersion:
				data = []byte{0x21}
			case ItemBufferSize:
				data = []byte{byte(s.buffer >> 8), byte(s.buffer)}
			case ItemProtocolVersion:
				if s.v == Version2 {
					data = []byte{0x20}
				}
			}

			if data != nil {
				body = appendWord(body, s.v, id)
				body = append(append(body, byte(len(data))), data...)
				n++
			}
		}

		res = append(appendWord(res, s.v, n), body...)

	case GetDatapointDescription, GetDatapointDescription2:
		if s.v == Version2 && m.Service == SetParameterByte {
			break
		}

		if m.Start > s.maxDP {
			s.inbound <- negative(ErrBadID, m.Start)
			return nil
		}

		if end := m.Start + m.Count - 1; end > s.maxDP {
			s.inbound <- negative(ErrBadID, s.maxDP+1)
			return nil
		}

		// Responses must fit into the buffer; the server reports the first datapoint that did not.
		capacity := int(s.buffer)
		if s.capacity > 0 {
			capacity = s.capacity
		}

		per := map[Version]int{Version1: 3, Version2: 5}[s.v]
		if fit := (capacity - s.v.headerSize()) / per; int(m.Count) > fit {
			s.inbound <- negative(ErrBufferTooSmall, m.Start+uint16(fit))
			return nil
		}

		res = appendWord(res, s.v, m.Count)
		for id := m.Start; id < m.Start+m.Count; id++ {
			if s.v == Version2 {
				res = appendWord(res, s.v, id)
			}

			res = append(res, byte(s.types[id]), 0x57, 1)
		}

	case GetDatapointValue:
		var n uint16
		var body []byte

		for id := m.Start; id < m.Start+m.Count; id++ {
			data, ok := s.values[id]
			if !ok && s.v == Version1 {
				s.inbound <- negative(ErrNoElement, id)
				return nil
			}

			body = appendWord(body, s.v, id)

			if s.v == Version1 {
				body = append(body, byte(len(data)))
			} else {
				state := byte(0)
				if ok {
					state = 0x10
				} else {
					data = []byte{0}
				}

				body = append(body, state, byte(len(data)))
			}

			body = append(body, data...)
			n++
		}

		res = append(appendWord(res, s.v, n), body...)

	case SetDatapointValue:
		r := reader{v: s.v, data: m.Body}
		s.lastWrite = nil

		for i := 0; i < int(m.Count); i++ {
			id := r.word()

			var cmd, length byte
			if s.v == Version1 {
				b := r.byte()
				cmd, length = b>>4, b&0x0F
			} else {
				cmd, length = r.byte(), r.byte()
			}

			s.lastWrite = append(s.lastWrite, SetValue{ID: id, Command: Command(cmd), Data: r.bytes(int(length))})
		}

		res = append(appendWord(res, s.v, 0), 0)
	}

	s.inbound <- res

	return nil
}

func TestClientDetectsVersion(t *testing.T) {
	for _, v := range []Version{Version1, Version2} {
		for _, linkMode := range []bool{false, true} {
			server := newFakeServer(v)
			server.linkMode = linkMode

			c, err := NewClient(server, Config{})
			if err != nil {
				t.Fatalf("v%d, link mode %v: %v", v, linkMode, err)
			}

			if c.Version() != v || c.buffer != 250 {
				t.Errorf("v%d, link mode %v: version %d, buffer %d", v, linkMode, c.Version(), c.buffer)
			}
		}
	}
}

func TestClientNoServer(t *testing.T) {
	server := newFakeServer(Version2)
	server.v = 3 // answers nothing

	if _, err := NewClient(server, Config{}); err != ErrNoServer {
		t.Errorf("NewClient: %v", err)
	}
}

func TestClientValues(t *testing.T) {
	for _, v := range []Version{Version1, Version2} {
		c, err := NewClient(newFakeServer(v), Config{Version: v})
		if err != nil {
			t.Fatal(err)
		}

		values, err := c.Values(1, 3, FilterAll)
		if err != nil {
			t.Fatalf("v%d: %v", v, err)
		}

		// Version 1 leaves out datapoint 2, which has no value.
		want := []uint16{1, 2, 3}
		if v == Version1 {
			want = []uint16{1, 3}
		}

		if len(values) != len(want) {
			t.Fatalf("v%d: %+v", v, values)
		}

		for i, value := range values {
			if value.ID != want[i] {
				t.Errorf("v%d: value %d has ID %d", v, i, value.ID)
			}
		}

		if !bytes.Equal(values[0].Data, []byte{0x0c, 0x1a}) {
			t.Errorf("v%d: DP 1 = % x", v, values[0].Data)
		}
	}
}

func TestClientDescriptionsInChunks(t *testing.T) {
	server := newFakeServer(Version2)
	server.buffer = 6 + 2*5 // two descriptions per response

	c, err := NewClient(server, Config{Version: Version2})
	if err != nil {
		t.Fatal(err)
	}

	server.requests = 0

	d, err := c.Descriptions(1, 5)
	if err != nil || len(d) != 5 || d[4].ID != 5 {
		t.Fatalf("Descriptions: %+v, %v", d, err)
	}

	if server.requests != 3 {
		t.Errorf("%d requests, want 3", server.requests)
	}

	if d[0].ValueType != Bytes2 || d[0].DPT != 1 {
		t.Errorf("DP 1: %+v", d[0])
	}
}

func TestClientSetValues(t *testing.T) {
	for _, v := range []Version{Version1, Version2} {
		server := newFakeServer(v)

		c, err := NewClient(server, Config{Version: v})
		if err != nil {
			t.Fatal(err)
		}

		err = c.SetValues(SetValue{ID: 2, Command: CommandSetAndSend, Data: []byte{1}}, SetValue{ID: 3, Command: CommandRead})
		if err != nil {
			t.Fatalf("v%d: %v", v, err)
		}

		w := server.lastWrite
		if len(w) != 2 || w[0].ID != 2 || w[0].Command != CommandSetAndSend || !bytes.Equal(w[0].Data, []byte{1}) || w[1].Command != CommandRead {
			t.Errorf("v%d: server got %+v", v, w)
		}
	}
}

func TestClientIndications(t *testing.T) {
	server := newFakeServer(Version2)

	c, err := NewClient(server, Config{Version: Version2})
	if err != nil {
		t.Fatal(err)
	}

	server.inbound <- []byte{0xf0, 0xc1, 0x00, 0x02, 0x00, 0x01, 0x00, 0x02, 0x18, 0x01, 0x01}

	select {
	case ind := <-c.Indications():
		if len(ind.Values) != 1 || len(ind.Raw) != 11 || ind.Values[0].ID != 2 || !ind.Values[0].State.Updated || ind.Values[0].Data[0] != 1 {
			t.Errorf("indication %+v", ind)
		}

	case <-time.After(time.Second):
		t.Fatal("no indication")
	}
}

func TestClientRangeLimits(t *testing.T) {
	server := newFakeServer(Version1)
	server.buffer = 240
	server.capacity = 100 // fewer than the buffer size suggests

	c, err := NewClient(server, Config{Version: Version1})
	if err != nil {
		t.Fatal(err)
	}

	// The range ends after the last datapoint of the server.
	d, err := c.Descriptions(1, 255)
	if err != nil || len(d) != 250 || d[249].ID != 250 {
		t.Fatalf("Descriptions: %d, %v", len(d), err)
	}

	for i, desc := range d {
		if desc.ID != uint16(i+1) {
			t.Fatalf("description %d has ID %d", i, desc.ID)
		}
	}

	if _, err := c.Descriptions(251, 1); err != ErrBadID {
		t.Errorf("beyond the last datapoint: %v", err)
	}
}

func TestClientRawRequest(t *testing.T) {
	c, err := NewClient(newFakeServer(Version2), Config{Version: Version2})
	if err != nil {
		t.Fatal(err)
	}

	res, err := c.Request([]byte{0xf0, 0x01, 0x00, 0x03, 0x00, 0x01})
	if err != nil || !bytes.Equal(res, []byte{0xf0, 0x81, 0x00, 0x03, 0x00, 0x01, 0x00, 0x03, 0x01, 0x21}) {
		t.Errorf("Request: % x, %v", res, err)
	}

	if _, err := c.Request([]byte{0x11, 0x00}); err != ErrMalformed {
		t.Errorf("not an ObjectServer message: %v", err)
	}

	// A version 1 request, which a version 2 server would ignore.
	if _, err := c.Request([]byte{0xf0, 0x01, 0x03, 0x01}); err != ErrMalformed {
		t.Errorf("version 1 request: %v", err)
	}
}

// resettingServer reports restarts like ft12.Conn.
type resettingServer struct {
	*fakeServer
	resets chan struct{}
}

func (s resettingServer) Resets() <-chan struct{} { return s.resets }

func TestClientRestoresModeAfterRestart(t *testing.T) {
	for _, v := range []Version{Version1, Version2} {
		server := resettingServer{newFakeServer(v), make(chan struct{}, 1)}

		c, err := NewClient(server, Config{Version: v, Timeout: 200 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}

		// The module restarts in link layer mode, e.g. after a download by ETS.
		c.mu.Lock()
		server.linkMode = true
		c.mu.Unlock()
		server.resets <- struct{}{}

		select {
		case ind := <-c.Indications():
			if !ind.Restarted {
				t.Errorf("v%d: indication %+v", v, ind)
			}
		case <-time.After(time.Second):
			t.Errorf("v%d: restart not indicated", v)
		}

		deadline := time.Now().Add(2 * time.Second)

		for {
			if _, err := c.ServerItems(ItemFirmwareVersion, 1); err == nil {
				break
			}

			if time.Now().After(deadline) {
				t.Fatalf("v%d: no ObjectServer after the restart", v)
			}
		}
	}
}

func TestClientRestoresModeWithoutIndication(t *testing.T) {
	server := newFakeServer(Version1)

	c, err := NewClient(server, Config{Version: Version1, Timeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}

	c.mu.Lock()
	server.linkMode = true // restarted without a reset indication
	c.mu.Unlock()

	if _, err := c.ServerItems(ItemFirmwareVersion, 1); err != nil {
		t.Errorf("after the restart: %v", err)
	}
}

func TestClientSetValuesInChunks(t *testing.T) {
	for _, v := range []Version{Version1, Version2} {
		server := newFakeServer(v)
		server.buffer = 30

		c, err := NewClient(server, Config{Version: v})
		if err != nil {
			t.Fatal(err)
		}

		server.requests = 0

		var values []SetValue
		for id := uint16(1); id <= 10; id++ {
			values = append(values, SetValue{ID: id, Command: CommandSetAndSend, Data: []byte{1, 2, 3, 4}})
		}

		if err := c.SetValues(values...); err != nil {
			t.Fatalf("v%d: %v", v, err)
		}

		// Version 2: 6 + 3*8 = 30 bytes per request; version 1: 4 + 4*6 = 28.
		want := map[Version]int{Version1: 3, Version2: 4}[v]
		if server.requests != want {
			t.Errorf("v%d: %d requests, want %d", v, server.requests, want)
		}

		if last := server.lastWrite; len(last) == 0 || last[len(last)-1].ID != 10 {
			t.Errorf("v%d: last request %+v", v, last)
		}
	}
}
