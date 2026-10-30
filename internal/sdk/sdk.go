// Package sdk serves the datapoints of a BAOS module as JSON, like dobaos: clients send requests
// on a stream socket, one JSON object per line, and get a response line each, and every client
// gets the broadcasts of datapoint values and server items. The methods, payloads and value
// formats are those of dobaos, without its Redis: the response carries the id of the request
// instead of going to a response channel.
//
//	-> {"id": 1, "method": "get value", "payload": [1, 2]}
//	<- {"id": 1, "method": "success", "payload": [{"id": 1, "value": true, "raw": "AQ=="}, ...]}
//	<- {"method": "datapoint value", "payload": [{"id": 2, "value": 21.5, "raw": "DDM="}]}
package sdk

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"slices"
	"sync"

	"github.com/shabunin/tiny-knx-go/baos"
)

// Version is the answer to the version method.
const Version = "tiny-knx-go sdk 1"

// A Request is a line that a client sends. ID is any JSON value, which the response repeats.
type Request struct {
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Payload json.RawMessage `json:"payload"`
}

// A Response answers a request; Method is "success" or "error", with the error code as the
// payload. Broadcasts have no ID and the method "datapoint value" or "server item".
type Response struct {
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Payload any             `json:"payload"`
}

// A Description describes a datapoint, as "get description" returns it.
type Description struct {
	ID            uint16 `json:"id"`
	Name          string `json:"name,omitempty"`
	Type          int    `json:"type"`           // the main DPT as dobaos gives it, e.g. 9, or 0 if unknown
	Size          int    `json:"size,omitempty"` // in bits; not with SetBare
	Priority      int    `json:"priority"`       // 0 system, 1 high, 2 alarm, 3 low
	Communication bool   `json:"communication"`
	Read          bool   `json:"read"`
	Write         bool   `json:"write"`
	ReadOnInit    bool   `json:"read_on_init"`
	Transmit      bool   `json:"transmit"`
	Update        bool   `json:"update"`
}

// A Value is the value of a datapoint: Value in the format of its type, which is missing for
// types without one, and Raw, the data. Valid is false for values that did not come from the bus
// or were not sent to it: the initial ones, which are zero, and those set with "put value".
type Value struct {
	ID      uint16 `json:"id"`
	Name    string `json:"name,omitempty"`
	Value   any    `json:"value,omitempty"`
	Raw     []byte `json:"raw"` // base64 in JSON
	Valid   *bool  `json:"valid,omitempty"`
	Success *bool  `json:"success,omitempty"`
}

// An Item is a server item, as "get server items" and its broadcasts return it.
type Item struct {
	ID    uint16 `json:"id"`
	Value []int  `json:"value"`
	Raw   []byte `json:"raw"`
}

// These are the errors of requests, which are the payloads of error responses.
var (
	ErrNoMethod           = errors.New("ERR_NO_METHOD_FIELD")
	ErrUnknownMethod      = errors.New("ERR_UNKNOWN_METHOD")
	ErrPayload            = errors.New("ERR_WRONG_PAYLOAD")
	ErrPayloadType        = errors.New("ERR_WRONG_PAYLOAD_TYPE")
	ErrDatapointNotFound  = errors.New("ERR_DATAPOINT_NOT_FOUND")
	ErrOutOfBounds        = errors.New("ERR_DATAPOINT_OUT_OF_BOUNDS") // an ID outside 1 to 1000
	ErrNoObjectServer     = errors.New("ERR_NO_OBJECT_SERVER")        // the module is in data link layer mode
	ErrValuePayload       = errors.New("ERR_WRONG_VALUE_PAYLOAD")
	ErrWrongRawValue      = errors.New("ERR_WRONG_RAW_VALUE")
	ErrDescriptionsFailed = errors.New("ERR_DESCRIPTIONS")
)

// baosErrors are the codes of the errors of the ObjectServer, as dobaos names them.
var baosErrors = map[baos.ErrorCode]string{
	1: "ERR_BAOS_INTERNAL", 2: "ERR_BAOS_NO_ELEMENT_FOUND", 3: "ERR_BAOS_BUFFER_TOO_SMALL",
	4: "ERR_BAOS_ITEM_NOT_WRITEABLE", 5: "ERR_BAOS_SERVICE_NOT_SUPPORTED",
	6: "ERR_BAOS_BAD_SERVICE_PARAMETER", 7: "ERR_BAOS_BAD_ID", 8: "ERR_BAOS_BAD_COMMAND/VALUE",
	9: "ERR_BAOS_BAD_LENGTH", 10: "ERR_BAOS_MESSAGE_INCONSISTENT", 11: "ERR_BAOS_BUSY",
}

func errorCode(err error) string {
	var code baos.ErrorCode
	if errors.As(err, &code) {
		if s, ok := baosErrors[code]; ok {
			return s
		}

		return "ERR_BAOS_UNKNOWN"
	}

	return err.Error()
}

// A Hub gives the ObjectServer, which changes when the module restarts or switches modes, and
// its indications; gateway.BAOSHub is one.
type Hub interface {
	Client() *baos.Client
	Subscribe() (<-chan baos.Indication, func())
}

// A Server serves the datapoints of the ObjectServer of a hub. Requests go to it one at a time.
type Server struct {
	hub       Hub
	namesFile string
	names     map[string]uint16
	idNames   map[uint16]string

	mu           sync.Mutex
	bare         bool
	client       *baos.Client // the one that the descriptions are of
	descriptions map[uint16]baos.Description
	stored       map[uint16]Value

	connsMu   sync.Mutex
	conns     map[*conn]bool
	listeners []func(Response)
}

// NewServer returns a server of the hub. Datapoints may have names, which requests may use
// instead of IDs; if namesFile is set, the names are read from it, {"name": ID, ...}, and the
// methods "set name" and "remove name" write it.
func NewServer(hub Hub, namesFile string) (*Server, error) {
	s := &Server{hub: hub, namesFile: namesFile, names: map[string]uint16{}, idNames: map[uint16]string{}, conns: map[*conn]bool{}}

	if namesFile != "" {
		data, err := os.ReadFile(namesFile)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}

		if len(data) > 0 {
			if err := json.Unmarshal(data, &s.names); err != nil {
				return nil, fmt.Errorf("%s: %v", namesFile, err)
			}
		}

		for name, id := range s.names {
			s.idNames[id] = name
		}
	}

	go s.broadcastIndications()

	return s, nil
}

// SetBare leaves out what dobaos does not send: the size in descriptions and the valid flag of
// values.
func (s *Server) SetBare(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.bare = on
}

// Listen calls f with every broadcast, e.g. to pass them on to other clients than those of Serve.
func (s *Server) Listen(f func(Response)) {
	s.connsMu.Lock()
	defer s.connsMu.Unlock()

	s.listeners = append(s.listeners, f)
}

// SetNames replaces the names of the datapoints, e.g. with names kept elsewhere than in the names
// file, which it does not write.
func (s *Server) SetNames(names map[string]uint16) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.names, s.idNames = map[string]uint16{}, map[uint16]string{}
	for name, id := range names {
		s.names[name], s.idNames[id] = id, name
	}
}

// setName gives a datapoint a name, or takes its name if name is empty. It is called with mu held.
func (s *Server) setName(id uint16, name string) error {
	if old, ok := s.idNames[id]; ok {
		delete(s.names, old)
		delete(s.idNames, id)
	}

	if name != "" {
		if other, ok := s.names[name]; ok {
			delete(s.idNames, other)
		}

		s.names[name], s.idNames[id] = id, name
	}

	if s.namesFile == "" {
		return nil
	}

	data, _ := json.MarshalIndent(s.names, "", "  ")

	return os.WriteFile(s.namesFile, append(data, '\n'), 0o644)
}

// Serve accepts clients on l until it closes.
func (s *Server) Serve(l net.Listener) error {
	for {
		nc, err := l.Accept()
		if err != nil {
			return err
		}

		go s.serveConn(nc)
	}
}

type conn struct {
	nc net.Conn
	mu sync.Mutex
}

func (c *conn) send(r Response) error {
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	_, err = c.nc.Write(append(line, '\n'))

	return err
}

func (s *Server) serveConn(nc net.Conn) {
	c := &conn{nc: nc}

	s.connsMu.Lock()
	s.conns[c] = true
	s.connsMu.Unlock()

	defer func() {
		s.connsMu.Lock()
		delete(s.conns, c)
		s.connsMu.Unlock()
		nc.Close()
	}()

	scanner := bufio.NewScanner(nc)
	scanner.Buffer(nil, 1<<20)

	for scanner.Scan() {
		var req Request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			c.send(Response{Method: "error", Payload: ErrPayload.Error()})
			continue
		}

		if c.send(s.Handle(req)) != nil {
			return
		}
	}
}

func (s *Server) broadcast(r Response) {
	s.connsMu.Lock()
	defer s.connsMu.Unlock()

	for c := range s.conns {
		if c.send(r) != nil {
			c.nc.Close()
		}
	}

	for _, f := range s.listeners {
		f(r)
	}
}

func (s *Server) broadcastIndications() {
	indications, _ := s.hub.Subscribe()

	for ind := range indications {
		s.mu.Lock()

		if ind.Restarted {
			s.client = nil // ETS may have changed the datapoints
		}

		var values []Value
		if len(ind.Values) > 0 && s.load() == nil {
			for _, v := range ind.Values {
				if d, ok := s.descriptions[v.ID]; ok {
					value := s.value(d, v)
					s.stored[v.ID] = value
					values = append(values, value)
				}
			}
		}

		s.mu.Unlock()

		if len(values) > 0 {
			s.broadcast(Response{Method: "datapoint value", Payload: values})
		}

		if len(ind.Items) > 0 {
			s.broadcast(Response{Method: "server item", Payload: items(ind.Items)})
		}
	}
}

// load reads the descriptions of the datapoints if the ObjectServer is new. It is called with mu
// held.
func (s *Server) load() error {
	c := s.hub.Client()
	if c == nil {
		return ErrNoObjectServer
	}

	if c == s.client {
		return nil
	}

	count := uint16(1000)
	if c.Version() == baos.Version1 {
		count = 255
	}

	descriptions, err := c.Descriptions(1, count)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrDescriptionsFailed, err)
	}

	s.descriptions = map[uint16]baos.Description{}
	for _, d := range descriptions {
		if d.DPT != baos.DPTDisabled || c.Version() == baos.Version2 {
			s.descriptions[d.ID] = d
		}
	}

	// The values may have changed while the module was away, e.g. in data link layer mode.
	s.client, s.stored = c, map[uint16]Value{}
	log.Printf("SDK: %d datapoints", len(s.descriptions))

	return nil
}

// Handle answers a request.
func (s *Server) Handle(req Request) Response {
	payload, err := s.handle(req)
	if err != nil {
		return Response{ID: req.ID, Method: "error", Payload: errorCode(err)}
	}

	return Response{ID: req.ID, Method: "success", Payload: payload}
}

func (s *Server) handle(req Request) (any, error) {
	if req.Method == "" {
		return nil, ErrNoMethod
	}

	var payload any
	if len(req.Payload) > 0 {
		if err := json.Unmarshal(req.Payload, &payload); err != nil {
			return nil, ErrPayload
		}
	}

	if req.Method == "version" {
		return Version, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if req.Method == "reset" {
		s.client = nil
		return true, s.load()
	}

	if err := s.load(); err != nil {
		return nil, err
	}

	c := s.client

	switch req.Method {
	case "get description":
		return s.each(payload, func(id uint16) any { return s.describe(s.descriptions[id]) })

	case "get value":
		return s.getValues(c, payload, false)

	case "get stored":
		return s.getValues(c, payload, true)

	case "set value":
		return s.setValues(c, payload, baos.CommandSetAndSend)

	case "put value":
		return s.setValues(c, payload, baos.CommandSet)

	case "read value":
		return s.readValues(c, payload)

	case "get programming mode":
		got, err := c.ServerItems(baos.ItemProgrammingMode, 1)
		if err != nil {
			return nil, err
		}

		if len(got) != 1 || len(got[0].Data) != 1 {
			return nil, ErrDatapointNotFound
		}

		return got[0].Data[0]&1 != 0, nil

	case "set programming mode":
		on, err := toBool(payload)
		if err != nil {
			return nil, ErrPayloadType
		}

		return true, c.SetServerItems(baos.Item{ID: baos.ItemProgrammingMode, Data: []byte{bit(on)}})

	case "get name":
		id, err := s.id(payload)
		if err != nil {
			return nil, err
		}

		if name, ok := s.idNames[id]; ok {
			return name, nil
		}

		return nil, nil

	case "set name":
		list := []any{payload}
		if p, ok := payload.([]any); ok {
			list = p
		}

		for _, item := range list {
			m, ok := item.(map[string]any)
			name, isString := m["name"].(string)
			if !ok || !isString || name == "" {
				return nil, ErrPayload
			}

			id, err := s.id(m["id"])
			if err != nil {
				return nil, err
			}

			if err := s.setName(id, name); err != nil {
				return nil, err
			}
		}

		return true, nil

	case "remove name":
		ids, _, err := s.ids(payload)
		if err != nil {
			return nil, err
		}

		for _, id := range ids {
			if err := s.setName(id, ""); err != nil {
				return nil, err
			}
		}

		return true, nil

	case "get server items":
		got, err := c.ServerItems(1, 17)
		if err != nil {
			return nil, err
		}

		return items(got), nil

	case "binary request":
		text, ok := payload.(string)
		if !ok {
			return nil, ErrPayloadType
		}

		req, err := base64.StdEncoding.DecodeString(text)
		if err != nil {
			return nil, ErrPayload
		}

		res, err := c.Request(req)
		if err != nil {
			return nil, err
		}

		return base64.StdEncoding.EncodeToString(res), nil
	}

	return nil, ErrUnknownMethod
}

func items(list []baos.Item) []Item {
	out := make([]Item, 0, len(list))

	for _, item := range list {
		value := make([]int, len(item.Data))
		for i, b := range item.Data {
			value[i] = int(b)
		}

		out = append(out, Item{ID: item.ID, Value: value, Raw: item.Data})
	}

	return out
}

func (s *Server) describe(d baos.Description) Description {
	f := d.Flags

	return Description{
		ID: d.ID, Name: s.idNames[d.ID], Type: mainType(d), Size: s.size(d), Priority: int(f.Priority()),
		Communication: f.Communication(), Read: f.Read(), Write: f.Write(), ReadOnInit: f.ReadOnInit(),
		Transmit: f.Transmit(), Update: f.UpdateOnResponse(),
	}
}

func (s *Server) size(d baos.Description) int {
	if s.bare {
		return 0
	}

	return d.ValueType.Bits()
}

// invalid returns the valid flag of a value that is not valid.
func (s *Server) invalid() *bool {
	if s.bare {
		return nil
	}

	return new(bool)
}

// value converts a value of the server. A value that a client set without sending it is not
// valid either, so the value is there anyway.
func (s *Server) value(d baos.Description, v baos.Value) Value {
	value := Value{ID: v.ID, Name: s.idNames[v.ID], Value: decodeValue(d, v.Data), Raw: v.Data}
	if !v.State.Valid {
		value.Valid = s.invalid()
	}

	return value
}

// id returns the datapoint of a JSON ID: a number or a name.
func (s *Server) id(v any) (uint16, error) {
	switch v := v.(type) {
	case float64:
		id := uint16(v)
		if float64(id) != v {
			return 0, ErrPayload
		}

		if id < 1 || id > 1000 {
			return 0, ErrOutOfBounds
		}

		if _, ok := s.descriptions[id]; !ok {
			return 0, ErrDatapointNotFound
		}

		return id, nil

	case string:
		if id, ok := s.names[v]; ok {
			if _, ok := s.descriptions[id]; ok {
				return id, nil
			}
		}

		return 0, ErrDatapointNotFound
	}

	return 0, ErrPayloadType
}

// ids returns the datapoints of a payload: null or "*" for all, an ID or a name for one, or an
// array of them. one tells that the payload named one datapoint, whose result is not an array.
func (s *Server) ids(payload any) (ids []uint16, one bool, err error) {
	switch p := payload.(type) {
	case nil:
		for id := range s.descriptions {
			ids = append(ids, id)
		}

		slices.Sort(ids)

		return ids, false, nil

	case string:
		if p == "*" {
			return s.ids(nil)
		}

	case []any:
		if len(p) == 0 {
			return nil, false, ErrPayload
		}

		for _, v := range p {
			id, err := s.id(v)
			if err != nil {
				return nil, false, err
			}

			ids = append(ids, id)
		}

		return ids, false, nil
	}

	id, err := s.id(payload)

	return []uint16{id}, true, err
}

// each returns the result of f for the datapoints of the payload.
func (s *Server) each(payload any, f func(id uint16) any) (any, error) {
	ids, one, err := s.ids(payload)
	if errors.Is(err, ErrOutOfBounds) {
		err = ErrDatapointNotFound // as dobaos answers for descriptions
	}

	if err != nil {
		return nil, err
	}

	if one {
		return f(ids[0]), nil
	}

	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = f(id)
	}

	return out, nil
}

// getValues reads the values of the datapoints of the payload from the server, or, if stored,
// those it has not read or been told yet.
func (s *Server) getValues(c *baos.Client, payload any, stored bool) (any, error) {
	ids, one, err := s.ids(payload)
	if err != nil {
		return nil, err
	}

	var missing []uint16
	for _, id := range ids {
		if _, ok := s.stored[id]; !ok || !stored {
			missing = append(missing, id)
		}
	}

	if len(missing) > 0 {
		low, high := slices.Min(missing), slices.Max(missing)

		values, err := c.Values(low, high-low+1, baos.FilterAll)
		if err != nil {
			return nil, err
		}

		for _, v := range values {
			if d, ok := s.descriptions[v.ID]; ok {
				s.stored[v.ID] = s.value(d, v)
			}
		}

		// Version 1 leaves out the values that are not known.
		for _, id := range missing {
			if _, ok := s.stored[id]; !ok {
				s.stored[id] = Value{ID: id, Valid: s.invalid()}
			}
		}
	}

	// Names may have changed since.
	named := func(id uint16) Value {
		v := s.stored[id]
		v.Name = s.idNames[id]

		return v
	}

	if one {
		return named(ids[0]), nil
	}

	out := make([]Value, len(ids))
	for i, id := range ids {
		out[i] = named(id)
	}

	return out, nil
}

// setValues sends the values of the payload, {"id": ID, "value": VALUE} or {"id": ID, "raw":
// BASE64} or an array of them, with the command, and broadcasts them.
func (s *Server) setValues(c *baos.Client, payload any, command baos.Command) (any, error) {
	list, one := []any{payload}, true
	if p, ok := payload.([]any); ok {
		list, one = p, false
	}

	if len(list) == 0 {
		return nil, ErrPayload
	}

	var (
		sets   []baos.SetValue
		values []Value
	)

	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, ErrPayloadType
		}

		id, err := s.id(m["id"])
		if err != nil {
			return nil, err
		}

		d := s.descriptions[id]

		var data []byte

		if raw, ok := m["raw"]; ok {
			text, ok := raw.(string)
			if !ok {
				return nil, ErrValuePayload
			}

			if data, err = base64.StdEncoding.DecodeString(text); err != nil || len(data) == 0 {
				return nil, ErrWrongRawValue
			}
		} else if v, ok := m["value"]; ok {
			if data, err = encodeValue(d, v); err != nil {
				return nil, err
			}
		} else {
			return nil, ErrValuePayload
		}

		sets = append(sets, baos.SetValue{ID: id, Command: command, Data: data})
		values = append(values, Value{ID: id, Name: s.idNames[id], Value: decodeValue(d, data), Raw: data, Success: ptr(true)})
	}

	// The server takes commands in the order of their IDs.
	sorted := slices.Clone(sets)
	slices.SortStableFunc(sorted, func(a, b baos.SetValue) int { return int(a.ID) - int(b.ID) })

	if err := c.SetValues(sorted...); err != nil {
		return nil, err
	}

	for _, v := range values {
		stored := v
		stored.Success = nil
		s.stored[v.ID] = stored
	}

	go s.broadcast(Response{Method: "datapoint value", Payload: withoutSuccess(values)})

	if one {
		return values[0], nil
	}

	return values, nil
}

func withoutSuccess(values []Value) []Value {
	out := slices.Clone(values)
	for i := range out {
		out[i].Success = nil
	}

	return out
}

// readValues sends a GroupValueRead for the datapoints of the payload; the responses come as
// broadcasts.
func (s *Server) readValues(c *baos.Client, payload any) (any, error) {
	ids, one, err := s.ids(payload)
	if err != nil {
		return nil, err
	}

	sets := make([]baos.SetValue, len(ids))
	for i, id := range ids {
		size := max(s.descriptions[id].ValueType.Size(), 1)
		sets[i] = baos.SetValue{ID: id, Command: baos.CommandRead, Data: make([]byte, size)}
	}

	sorted := slices.Clone(sets)
	slices.SortStableFunc(sorted, func(a, b baos.SetValue) int { return int(a.ID) - int(b.ID) })

	if err := c.SetValues(sorted...); err != nil {
		return nil, err
	}

	if one {
		return true, nil
	}

	type result struct {
		ID      uint16 `json:"id"`
		Success bool   `json:"success"`
	}

	out := make([]result, len(ids))
	for i, id := range ids {
		out[i] = result{ID: id, Success: true}
	}

	return out, nil
}

func ptr[T any](v T) *T { return &v }
