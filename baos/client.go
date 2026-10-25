package baos

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shabunin/tiny-knx-go/cemi"
)

var (
	// ErrTimeout is returned when the server does not answer a request in time.
	ErrTimeout = errors.New("baos: no response")

	// ErrNoServer is returned by NewClient when the module answers in neither version, even after
	// it was switched to ObjectServer mode.
	ErrNoServer = errors.New("baos: no object server found")

	// ErrClosed is returned when the transport has closed.
	ErrClosed = errors.New("baos: closed")

	// ErrNotSupported is returned for services that the protocol version does not have.
	ErrNotSupported = errors.New("baos: not supported by this protocol version")
)

// A Transport carries ObjectServer messages, e.g. ft12.Conn for a module on a serial port.
type Transport interface {
	Send(payload []byte) error
	Inbound() <-chan []byte
}

// A Resetter is a Transport that reports when the module restarted, e.g. ft12.Conn. A module
// restarts in its default mode, and the Client switches it back to ObjectServer mode.
type Resetter interface {
	Resets() <-chan struct{}
}

// Config configures a Client.
type Config struct {
	// Version is the protocol version of the server. Zero detects it.
	Version Version

	// Timeout is how long to wait for a response.
	Timeout time.Duration

	// Indications is the number of indications that are buffered for the reader. When the queue is
	// full, new indications are dropped.
	Indications int
}

// DefaultConfig works with Weinzierl BAOS modules.
var DefaultConfig = Config{
	Timeout:     2 * time.Second,
	Indications: 32,
}

// detectTimeout limits the wait for an answer while the version is not known. Servers answer
// within some 30 ms and ignore requests of the other version.
const detectTimeout = 500 * time.Millisecond

// An Indication reports changed datapoint values or server items.
type Indication struct {
	Values []Value
	Items  []Item
	Raw    []byte // the message as received
}

// A Client talks to an ObjectServer. It sends one request at a time.
type Client struct {
	t       Transport
	config  Config
	version atomic.Uint32
	buffer  int // largest message the server takes and sends

	mu          sync.Mutex
	responses   chan []byte
	indications chan Indication
}

// These messages switch a Weinzierl module to ObjectServer mode: cEMI modules (kBerry) through
// the communication mode of the cEMI server object, EMI2 modules (BAOS 820) through PEI_Switch to
// their normal mode, in which they run the ObjectServer. Each module ignores the other message.
var (
	cemiObjectServerMode = packCEMI(&cemi.MPropWriteReq{MProp: cemi.MProp{
		ObjectType: cemi.ObjectTypeCEMIServer, Instance: 1, PropertyID: cemi.PIDCommMode,
		Count: 1, Start: 1, Data: []byte{cemi.CommModeObjectServer},
	}})
	emiNormalMode = []byte{0xA9, 0x1E, 0x12, 0x34, 0x56, 0x78, 0x9A}
)

func packCEMI(msg cemi.Message) []byte {
	b := make([]byte, cemi.Size(msg))
	cemi.Pack(b, msg)

	return b
}

// NewClient connects to the ObjectServer behind t. Without a version in config, it asks in both
// versions; if the module answers neither, it is switched to ObjectServer mode first, as it may
// still be in data link layer mode.
func NewClient(t Transport, config Config) (*Client, error) {
	if config.Timeout <= 0 {
		config.Timeout = DefaultConfig.Timeout
	}

	if config.Indications <= 0 {
		config.Indications = DefaultConfig.Indications
	}

	c := &Client{
		t:           t,
		config:      config,
		buffer:      64,
		responses:   make(chan []byte, 1),
		indications: make(chan Indication, config.Indications),
	}

	go c.serve()

	if r, ok := t.(Resetter); ok {
		go c.restore(r.Resets())
	}

	if err := c.detect(); err != nil {
		return nil, err
	}

	// The buffer size limits how many elements a request asks for.
	if items, err := c.ServerItems(ItemBufferSize, 1); err == nil && len(items) == 1 && len(items[0].Data) == 2 {
		if size := int(items[0].Data[0])<<8 | int(items[0].Data[1]); size > c.Version().headerSize() {
			c.buffer = size
		}
	}

	return c, nil
}

func (c *Client) detect() error {
	if c.config.Version != 0 {
		c.version.Store(uint32(c.config.Version))
		return nil
	}

	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			if err := c.t.Send(cemiObjectServerMode); err != nil {
				return err
			}

			if err := c.t.Send(emiNormalMode); err != nil {
				return err
			}
		}

		// Item 16, the protocol version, exists in version 2 only; item 3 is the firmware version.
		for _, probe := range []struct {
			v    Version
			item uint16
		}{{Version2, ItemProtocolVersion}, {Version1, ItemFirmwareVersion}} {
			req, _ := AppendRangeRequest(nil, probe.v, GetServerItem, probe.item, 1)

			_, err := c.request(probe.v, req, detectTimeout)
			if err == nil {
				c.version.Store(uint32(probe.v))
				return nil
			}

			if err != ErrTimeout {
				return err
			}
		}
	}

	return ErrNoServer
}

// restore switches the module back to ObjectServer mode after each restart, e.g. when ETS
// restarts it after a download. Each module ignores the message of the other kind.
func (c *Client) restore(resets <-chan struct{}) {
	for range resets {
		time.Sleep(restartDelay)

		c.mu.Lock()
		c.t.Send(cemiObjectServerMode)
		c.t.Send(emiNormalMode)
		c.mu.Unlock()
	}
}

// restartDelay lets the module finish starting before it is switched.
const restartDelay = 100 * time.Millisecond

// Version returns the protocol version of the server.
func (c *Client) Version() Version {
	return Version(c.version.Load())
}

// Indications returns the channel of datapoint value and server item indications. It is closed
// when the transport closes.
func (c *Client) Indications() <-chan Indication {
	return c.indications
}

func (c *Client) serve() {
	defer close(c.responses)
	defer close(c.indications)

	for payload := range c.t.Inbound() {
		if len(payload) < 2 || payload[0] != MainService {
			continue
		}

		switch payload[1] {
		case DatapointValueIndication, ServerItemIndication:
			c.indicate(payload)

		default:
			// Replace an unclaimed response, e.g. one that arrived after its request timed out.
			for {
				select {
				case c.responses <- payload:
				default:
					select {
					case <-c.responses:
					default:
					}

					continue
				}

				break
			}
		}
	}
}

func (c *Client) indicate(payload []byte) {
	v := c.Version()
	if v == 0 {
		return
	}

	m, err := Parse(v, payload)
	if err != nil {
		return
	}

	ind := Indication{Raw: payload}

	if m.Service == DatapointValueIndication {
		ind.Values, err = m.Values(v)
	} else {
		ind.Items, err = m.Items(v)
	}

	if err != nil {
		return
	}

	select {
	case c.indications <- ind:
	default:
	}
}

// request sends a request and returns its response.
func (c *Client) request(v Version, req []byte, timeout time.Duration) (Message, error) {
	payload, err := c.exchange(req, timeout)
	if err != nil {
		return Message{}, err
	}

	return Parse(v, payload)
}

// Request sends a raw ObjectServer request and returns the raw response, e.g. for a proxy that
// passes the messages of its own clients. A request in the format of the other protocol version
// fails at once with ErrMalformed, as the server would ignore it: clients that detect the version
// ask in both.
func (c *Client) Request(req []byte) ([]byte, error) {
	if len(req) < 2 || req[0] != MainService || !c.Version().fits(req) {
		return nil, ErrMalformed
	}

	return c.exchange(req, c.config.Timeout)
}

// exchange sends a request and waits for the response with its subservice. If the server does not
// answer, the module may have restarted in its default mode without telling (not every module sends
// a reset indication): exchange switches it to ObjectServer mode and tries once more.
func (c *Client) exchange(req []byte, timeout time.Duration) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	res, err := c.send(req, timeout)
	if err != ErrTimeout || c.Version() == 0 {
		return res, err
	}

	c.t.Send(cemiObjectServerMode)
	c.t.Send(emiNormalMode)
	time.Sleep(restartDelay)

	return c.send(req, timeout)
}

func (c *Client) send(req []byte, timeout time.Duration) ([]byte, error) {
	select {
	case <-c.responses:
	default:
	}

	if err := c.t.Send(req); err != nil {
		return nil, err
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case payload, open := <-c.responses:
			if !open {
				return nil, ErrClosed
			}

			if payload[1] == req[1]|ResponseFlag {
				return payload, nil
			}

		case <-timer.C:
			return nil, ErrTimeout
		}
	}
}

func (c *Client) rangeRequest(service byte, start, count uint16) (Message, error) {
	v := c.Version()

	req, err := AppendRangeRequest(nil, v, service, start, count)
	if err != nil {
		return Message{}, err
	}

	return c.request(v, req, c.config.Timeout)
}

// chunk returns how many elements of at most size bytes fit into a response.
func (c *Client) chunk(size int) uint16 {
	n := (c.buffer - c.Version().headerSize()) / size
	if v := c.Version(); v == Version1 && n > 0xFF {
		n = 0xFF
	}

	return uint16(max(n, 1))
}

// ServerItems reads the server items from start to start+count-1. Items that the server does not
// have are missing from the result.
func (c *Client) ServerItems(start, count uint16) ([]Item, error) {
	m, err := c.rangeRequest(GetServerItem, start, count)
	if err != nil {
		return nil, err
	}

	return m.Items(c.Version())
}

// SetServerItems writes server items, which must have ascending IDs.
func (c *Client) SetServerItems(items ...Item) error {
	v := c.Version()

	req, err := AppendSetServerItem(nil, v, items)
	if err != nil {
		return err
	}

	m, err := c.request(v, req, c.config.Timeout)
	if err != nil {
		return err
	}

	return m.Err()
}

// Descriptions reads the descriptions of the datapoints from start to start+count-1. In version 2
// the result has only the datapoints that ETS configured; in version 1 it has all, with DPT 0 for
// those not configured. The range may go beyond the last datapoint of the server.
func (c *Client) Descriptions(start, count uint16) ([]Description, error) {
	v := c.Version()
	service, size := byte(GetDatapointDescription), 5

	if v == Version1 {
		service, size = GetDatapointDescription2, 3
	}

	var all []Description

	err := c.forRange(start, count, c.chunk(size), func(start, n uint16) (Message, error) {
		m, err := c.rangeRequest(service, start, n)

		// Older version 1 firmware has no descriptions with the DPT.
		if err == nil && m.Err() == ErrServiceNotSupported && service == GetDatapointDescription2 {
			service = GetDatapointDescription
			m, err = c.rangeRequest(service, start, n)
		}

		return m, err
	}, func(m *Message) error {
		descriptions, err := m.Descriptions(v)
		all = append(all, descriptions...)

		return err
	})

	return all, err
}

// forRange requests the elements from start to start+count-1 in chunks with get and passes the
// responses to use. Servers report the first element that they could not take in a negative
// response: forRange takes smaller chunks if the buffer was too small, ends the range at the last
// element of the server, and skips elements without a value.
func (c *Client) forRange(start, count, chunk uint16, get func(start, n uint16) (Message, error), use func(*Message) error) error {
	first := start
	end := uint32(start) + uint32(count)
	limit := uint32(0) // for the next request only

	for uint32(start) < end {
		n := min(uint32(chunk), end-uint32(start))
		if limit > 0 {
			n, limit = min(n, limit), 0
		}

		m, err := get(start, uint16(n))
		if err == nil {
			err = m.Err()
		}

		if err == nil {
			if err := use(&m); err != nil {
				return err
			}

			start += uint16(n)
			continue
		}

		code, ok := err.(ErrorCode)
		if !ok {
			return err
		}

		inside := m.Start > start && uint32(m.Start) < uint32(start)+n

		switch {
		case code == ErrBufferTooSmall && inside:
			chunk = m.Start - start

		case (code == ErrBadID || code == ErrBadParameter) && inside:
			end = uint32(m.Start)

		case (code == ErrBadID || code == ErrBadParameter) && start > first:
			return nil // no more elements

		case code == ErrNoElement && inside:
			limit = uint32(m.Start - start)

		case code == ErrNoElement && m.Start == start && c.Version() == Version1:
			start++ // version 1 reports one element without a value

		case code == ErrNoElement:
			start += uint16(n) // version 2: none of the chunk configured

		default:
			return err
		}
	}

	return nil
}

// DescriptionStrings reads the description strings of the datapoints from start to
// start+count-1. BAOS modules do not have them (ErrServiceNotSupported).
func (c *Client) DescriptionStrings(start, count uint16) ([]string, error) {
	v := c.Version()
	length := 0

	if v == Version1 {
		items, err := c.ServerItems(ItemDescriptionLength, 1)
		if err != nil {
			return nil, err
		}

		if len(items) != 1 || len(items[0].Data) != 2 {
			return nil, ErrServiceNotSupported
		}

		length = int(items[0].Data[0])<<8 | int(items[0].Data[1])
	}

	m, err := c.rangeRequest(GetDescriptionString, start, count)
	if err != nil {
		return nil, err
	}

	return m.Strings(v, length)
}

// Values reads the values of the datapoints from start to start+count-1. Version 1 has no filter;
// it does not report datapoints without a value, which are missing from the result.
func (c *Client) Values(start, count uint16, filter Filter) ([]Value, error) {
	v := c.Version()

	var all []Value

	err := c.forRange(start, count, c.chunk(14+v.headerSize()-2), func(start, n uint16) (Message, error) {
		req, err := AppendGetDatapointValue(nil, v, start, n, filter)
		if err != nil {
			return Message{}, err
		}

		return c.request(v, req, c.config.Timeout)
	}, func(m *Message) error {
		values, err := m.Values(v)
		all = append(all, values...)

		return err
	})

	return all, err
}

// SetValues sends datapoint commands, which must have ascending IDs.
func (c *Client) SetValues(values ...SetValue) error {
	v := c.Version()

	req, err := AppendSetDatapointValue(nil, v, values)
	if err != nil {
		return err
	}

	m, err := c.request(v, req, c.config.Timeout)
	if err != nil {
		return err
	}

	return m.Err()
}

// ParameterBytes reads the parameter bytes from start to start+count-1. They are numbered from 1.
func (c *Client) ParameterBytes(start, count uint16) ([]byte, error) {
	var all []byte

	err := c.forRange(start, count, c.chunk(1), func(start, n uint16) (Message, error) {
		return c.rangeRequest(GetParameterByte, start, n)
	}, func(m *Message) error {
		b, err := m.ParameterBytes()
		all = append(all, b...)

		return err
	})

	return all, err
}

// SetParameterBytes writes parameter bytes (version 2.2 and later). Without data, it asks the
// server to save the written bytes.
func (c *Client) SetParameterBytes(start uint16, data []byte) error {
	if c.Version() != Version2 {
		return ErrNotSupported
	}

	m, err := c.request(Version2, AppendSetParameterByte(nil, start, data), c.config.Timeout)
	if err != nil {
		return err
	}

	return m.Err()
}
