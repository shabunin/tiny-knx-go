package sdk

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/shabunin/tiny-knx-go/baos"
	"github.com/shabunin/tiny-knx-go/internal/gateway"
)

func TestValues(t *testing.T) {
	tests := []struct {
		dpt   baos.DPT
		data  []byte
		value string // JSON
	}{
		{1, []byte{1}, `true`},
		{2, []byte{2}, `{"control":true,"value":false}`},
		{3, []byte{0x0B}, `{"direction":1,"step":3}`},
		{4, []byte{'A'}, `"A"`},
		{5, []byte{200}, `200`},
		{6, []byte{0xF6}, `-10`},
		{7, []byte{0x12, 0x34}, `4660`},
		{8, []byte{0xFF, 0xFE}, `-2`},
		{9, []byte{0x0C, 0x33}, `21.5`},
		{9, []byte{0x87, 0x9C}, `-1`},
		{9, []byte{0x7F, 0xFE}, `670433.28`},
		{10, []byte{0x2C, 30, 15}, `{"day":1,"hour":12,"minutes":30,"seconds":15}`},
		{11, []byte{26, 10, 26}, `{"day":26,"month":10,"year":2026}`},
		{12, []byte{0, 1, 0, 0}, `65536`},
		{13, []byte{0xFF, 0xFF, 0xFF, 0xFF}, `-1`},
		{14, []byte{0x41, 0xAC, 0, 0}, `21.5`},
		{16, append([]byte("KNX is OK"), 0, 0, 0, 0, 0), `"KNX is OK"`},
		{17, []byte{5}, `5`},
		{18, []byte{0x85}, `{"learn":1,"number":5}`},
	}

	for _, tt := range tests {
		d := baos.Description{DPT: tt.dpt}

		got, _ := json.Marshal(decodeValue(d, tt.data))
		if string(got) != tt.value {
			t.Errorf("DPT %d: % x decodes to %s, want %s", tt.dpt, tt.data, got, tt.value)
		}

		var v any
		json.Unmarshal([]byte(tt.value), &v)

		data, err := encodeValue(d, v)
		if err != nil || !bytes.Equal(data, tt.data) {
			t.Errorf("DPT %d: %s encodes to % x, %v; want % x", tt.dpt, tt.value, data, err, tt.data)
		}
	}

	bad := []struct {
		dpt   baos.DPT
		value any
	}{
		{5, 256.0}, {5, 1.5}, {5, "x"}, {9, 700000.0}, {10, map[string]any{"day": 1.0}}, {4, "AB"},
	}

	for _, tt := range bad {
		if data, err := encodeValue(baos.Description{DPT: tt.dpt}, tt.value); err == nil {
			t.Errorf("DPT %d: %v encodes to % x", tt.dpt, tt.value, data)
		}
	}

	if _, err := encodeValue(baos.Description{DPT: 19}, 1.0); err != ErrDPTNotSupported {
		t.Errorf("DPT 19: %v", err)
	}
}

// objectServer is an ObjectServer of version 2 with three datapoints: 1 (DPT 1), 2 (DPT 9) and
// 3 (DPT 10, without a value).
type objectServer struct {
	mu      sync.Mutex
	values  map[uint16][]byte
	prog    byte
	writes  []baos.SetValue
	inbound chan []byte
}

var descriptions = []struct {
	t   baos.ValueType
	dpt baos.DPT
}{{baos.Bits1, 1}, {baos.Bytes2, 9}, {baos.Bytes3, 10}}

func newObjectServer() *objectServer {
	return &objectServer{values: map[uint16][]byte{1: {0}, 2: {0x0C, 0x1A}}, inbound: make(chan []byte, 16)}
}

func (s *objectServer) Inbound() <-chan []byte { return s.inbound }

func (s *objectServer) Send(req []byte) error {
	if len(req) < 6 || req[0] != baos.MainService {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	service := req[1]
	start, count := binary.BigEndian.Uint16(req[2:]), binary.BigEndian.Uint16(req[4:])
	res := []byte{baos.MainService, service | baos.ResponseFlag, req[2], req[3]}

	word := func(b []byte, x uint16) []byte { return binary.BigEndian.AppendUint16(b, x) }
	last := uint16(len(descriptions))

	switch service {
	case baos.GetServerItem:
		var n uint16
		var body []byte

		for id := start; id < start+count; id++ {
			var data []byte

			switch id {
			case baos.ItemBufferSize:
				data = []byte{0, 250}
			case baos.ItemProgrammingMode:
				data = []byte{s.prog}
			}

			if data != nil {
				body = append(append(word(body, id), byte(len(data))), data...)
				n++
			}
		}

		res = append(word(res, n), body...)

	case baos.SetServerItem:
		if binary.BigEndian.Uint16(req[6:]) == baos.ItemProgrammingMode {
			s.prog = req[9]
		}

		res = append(word(res, 0), 0)

	case baos.GetDatapointDescription:
		if start+count-1 > last {
			res = append(word(word(res[:2], last+1), 0), byte(baos.ErrBadID))
			break
		}

		res = word(res, count)
		for id := start; id < start+count; id++ {
			d := descriptions[id-1]
			res = append(word(res, id), byte(d.t), 0x57, byte(d.dpt))
		}

	case baos.GetDatapointValue:
		var n uint16
		var body []byte

		for id := start; id < start+count && id <= last; id++ {
			data, ok := s.values[id]
			state := byte(0x10)
			if !ok {
				data, state = make([]byte, descriptions[id-1].t.Size()), 0
			}

			body = append(append(word(body, id), state, byte(len(data))), data...)
			n++
		}

		res = append(word(res, n), body...)

	case baos.SetDatapointValue:
		body := req[6:]
		for i := 0; i < int(count); i++ {
			id, cmd, n := binary.BigEndian.Uint16(body), baos.Command(body[2]), int(body[3])
			data := body[4 : 4+n]
			body = body[4+n:]

			s.writes = append(s.writes, baos.SetValue{ID: id, Command: cmd, Data: data})
			if cmd&baos.CommandSet != 0 {
				s.values[id] = data
			}
		}

		res = append(word(res, 0), 0)

	default:
		return nil
	}

	s.inbound <- res

	return nil
}

func startServer(t *testing.T, hub *gateway.BAOSHub) *Client {
	t.Helper()

	l, err := gateway.ListenBAOS(filepath.Join(t.TempDir(), "sdk.sock"), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })

	names := filepath.Join(t.TempDir(), "names.json")
	os.WriteFile(names, []byte(`{"temperature": 2}`), 0o644)

	server, err := NewServer(hub, names)
	if err != nil {
		t.Fatal(err)
	}

	go server.Serve(l)

	c, err := Dial(l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })

	return c
}

func call(t *testing.T, c *Client, method string, payload any, out any) {
	t.Helper()

	res, err := c.Call(method, payload)
	if err != nil {
		t.Fatalf("%s %v: %v", method, payload, err)
	}

	if err := json.Unmarshal(res, out); err != nil {
		t.Fatalf("%s %v: %s: %v", method, payload, res, err)
	}
}

func broadcast(t *testing.T, c *Client) string {
	t.Helper()

	select {
	case m := <-c.Broadcasts():
		return m.Method + " " + string(m.Payload)
	case <-time.After(time.Second):
		t.Fatal("no broadcast")
	}

	return ""
}

func TestServer(t *testing.T) {
	server := newObjectServer()

	bc, err := baos.NewClient(server, baos.Config{Version: baos.Version2})
	if err != nil {
		t.Fatal(err)
	}

	hub := gateway.NewBAOSHub()
	hub.Use(bc)

	c := startServer(t, hub)

	var list []Description
	call(t, c, "get description", nil, &list)

	if len(list) != 3 || list[1].Type != "dpt9" || list[1].Name != "temperature" || list[1].Size != 16 || !list[1].Write || list[1].Read {
		t.Fatalf("descriptions %+v", list)
	}

	var one Description
	call(t, c, "get description", "temperature", &one)

	if one.ID != 2 {
		t.Errorf("description of a name: %+v", one)
	}

	var values []map[string]any
	call(t, c, "get value", nil, &values)

	want := []map[string]any{
		{"id": 1.0, "value": false, "raw": "AA=="},
		{"id": 2.0, "name": "temperature", "value": 21.0, "raw": "DBo="},
		{"id": 3.0, "value": map[string]any{"day": 0.0, "hour": 0.0, "minutes": 0.0, "seconds": 0.0}, "valid": false, "raw": "AAAA"},
	}

	if !reflect.DeepEqual(values, want) {
		t.Errorf("values %v, want %v", values, want)
	}

	var set []map[string]any
	call(t, c, "set value", []any{
		map[string]any{"id": "temperature", "value": 21.5},
		map[string]any{"id": 1, "raw": "AQ=="},
	}, &set)

	if len(set) != 2 || set[0]["value"] != 21.5 || set[1]["value"] != true || set[0]["success"] != true {
		t.Errorf("set %v", set)
	}

	server.mu.Lock()
	writes := server.writes
	server.mu.Unlock()

	if len(writes) != 2 || writes[0].ID != 1 || writes[1].ID != 2 || !bytes.Equal(writes[1].Data, []byte{0x0C, 0x33}) || writes[1].Command != baos.CommandSetAndSend {
		t.Errorf("writes %+v", writes)
	}

	if b := broadcast(t, c); b != `datapoint value [{"id":2,"name":"temperature","value":21.5,"raw":"DDM="},{"id":1,"value":true,"raw":"AQ=="}]` {
		t.Errorf("broadcast %s", b)
	}

	var stored map[string]any
	call(t, c, "get stored", 2, &stored)

	if stored["value"] != 21.5 {
		t.Errorf("stored %v", stored)
	}

	// A value from the bus: DP 3 = Monday 12:30:15.
	server.inbound <- []byte{0xF0, 0xC1, 0, 3, 0, 1, 0, 3, 0x10, 3, 0x2C, 30, 15}

	if b := broadcast(t, c); b != `datapoint value [{"id":3,"value":{"day":1,"hour":12,"minutes":30,"seconds":15},"raw":"LB4P"}]` {
		t.Errorf("broadcast %s", b)
	}

	var ok bool
	call(t, c, "set programming mode", true, &ok)
	call(t, c, "get programming mode", nil, &ok)

	if !ok {
		t.Error("programming mode is off")
	}

	call(t, c, "read value", 2, &ok)

	server.mu.Lock()
	read := server.writes[len(server.writes)-1]
	server.mu.Unlock()

	if read.ID != 2 || read.Command != baos.CommandRead || len(read.Data) != 2 {
		t.Errorf("read %+v", read)
	}

	call(t, c, "set name", map[string]any{"id": 1, "name": "light"}, &ok)

	var name string
	call(t, c, "get name", "light", &name)

	var light map[string]any
	call(t, c, "get stored", "light", &light)

	if name != "light" || light["name"] != "light" || light["id"] != 1.0 {
		t.Errorf("name %q, stored %v", name, light)
	}

	call(t, c, "remove name", "light", &ok)

	if _, err := c.Call("get value", "light"); err == nil {
		t.Error("removed name still works")
	}

	for _, tt := range []struct {
		method  string
		payload any
		err     string
	}{
		{"get value", 9, "ERR_DATAPOINT_NOT_FOUND"},
		{"get value", "nothing", "ERR_DATAPOINT_NOT_FOUND"},
		{"set value", map[string]any{"id": 1}, "ERR_WRONG_VALUE_PAYLOAD"},
		{"set value", map[string]any{"id": 2, "value": "warm"}, "ERR_WRONG_VALUE_TYPE"},
		{"dance", nil, "ERR_UNKNOWN_METHOD"},
	} {
		if _, err := c.Call(tt.method, tt.payload); err == nil || err.Error() != tt.err {
			t.Errorf("%s %v: %v, want %s", tt.method, tt.payload, err, tt.err)
		}
	}
}

func TestServerWithoutObjectServer(t *testing.T) {
	c := startServer(t, gateway.NewBAOSHub())

	if _, err := c.Call("get value", nil); err == nil || err.Error() != "ERR_NO_OBJECT_SERVER" {
		t.Errorf("%v", err)
	}

	var version string
	call(t, c, "version", nil, &version)

	if version != Version {
		t.Errorf("version %q", version)
	}
}
