package transport

import (
	"errors"
	"sync"
	"time"

	"github.com/shabunin/tiny-knx-go/apdu"
	"github.com/shabunin/tiny-knx-go/cemi"
	"github.com/shabunin/tiny-knx-go/link"
)

var (
	// ErrConnectFailed is returned when a device does not accept a connection.
	ErrConnectFailed = errors.New("transport: connection failed")

	// ErrDisconnected is returned when the connection closes during a request.
	ErrDisconnected = errors.New("transport: disconnected")

	// ErrNoResponse is returned when a device acknowledged a request but did not answer it.
	ErrNoResponse = errors.New("transport: no response")

	// ErrRejected is returned when a device answers a request with a negative response.
	ErrRejected = errors.New("transport: request rejected by device")
)

// A Client talks to one device at a time over a connection, the way management tools such as ETS
// do: it connects, sends requests, waits for their responses and disconnects.
//
// The client owns the link while it runs: it reads all inbound frames.
type Client struct {
	link link.Link
	conn *Conn

	// ResponseTimeout is how long to wait for a response after the device acknowledged a request.
	ResponseTimeout time.Duration

	callMu   sync.Mutex // one call at a time
	commands chan command
	events   chan Event // events for the current call
	done     chan struct{}
	stopped  chan struct{}
	once     sync.Once
}

type command struct {
	run  func(now time.Time)
	done chan struct{}
}

// NewClient starts a client on the link.
func NewClient(l link.Link, config Config) *Client {
	c := &Client{
		link:            l,
		conn:            NewConn(config),
		ResponseTimeout: 3 * time.Second,
		commands:        make(chan command),
		events:          make(chan Event, 16),
		done:            make(chan struct{}),
		stopped:         make(chan struct{}),
	}

	go c.run()

	return c
}

// Close disconnects and stops the client. It does not close the link.
func (c *Client) Close() {
	c.once.Do(func() {
		c.exec(func(time.Time) { c.conn.Disconnect() })
		close(c.done)
		<-c.stopped
	})
}

// Connect opens a connection to the device at addr.
func (c *Client) Connect(addr cemi.IndividualAddr) error {
	c.callMu.Lock()
	defer c.callMu.Unlock()

	c.discardEvents()

	if !c.exec(func(now time.Time) { c.conn.Connect(now, addr) }) {
		return link.ErrClosed
	}

	for {
		event, ok := c.nextEvent(nil)
		if !ok {
			return link.ErrClosed
		}

		switch event.Kind {
		case EventConnected:
			return nil
		case EventDisconnected:
			return ErrConnectFailed
		}
	}
}

// Disconnect closes the connection.
func (c *Client) Disconnect() {
	c.callMu.Lock()
	defer c.callMu.Unlock()

	c.exec(func(time.Time) { c.conn.Disconnect() })
}

// Request sends an APDU over the connection and waits for the response for which match returns
// true. With a nil match it returns once the device acknowledged the request.
func (c *Client) Request(req apdu.APDU, match func(apdu.APDU) bool) (apdu.APDU, error) {
	c.callMu.Lock()
	defer c.callMu.Unlock()

	c.discardEvents()

	var err error
	if !c.exec(func(now time.Time) { err = c.conn.Send(now, apdu.Encode(req)) }) {
		return nil, link.ErrClosed
	}

	if err != nil {
		return nil, err
	}

	// The device answers after it acknowledged the request, but if our acknowledgement of the
	// T_ACK got lost, the response may come first.
	var timeout <-chan time.Time

	for {
		event, ok := c.nextEvent(timeout)
		if !ok {
			if timeout != nil {
				return nil, ErrNoResponse
			}

			return nil, link.ErrClosed
		}

		switch event.Kind {
		case EventConfirmed:
			if match == nil {
				return nil, nil
			}

			timeout = time.After(c.ResponseTimeout)

		case EventData:
			response, err := apdu.Decode(event.TPDU.(*cemi.AppData))
			if err == nil && match != nil && match(response) {
				return response, nil
			}

		case EventDisconnected:
			return nil, ErrDisconnected
		}
	}
}

// DeviceDescriptor reads descriptor type 0, the mask version, e.g. 0x07B0 for a System B device.
func (c *Client) DeviceDescriptor() (uint16, error) {
	response, err := c.Request(apdu.DeviceDescriptorRead{}, func(a apdu.APDU) bool {
		_, ok := a.(apdu.DeviceDescriptorResponse)
		return ok
	})
	if err != nil {
		return 0, err
	}

	descriptor := response.(apdu.DeviceDescriptorResponse)
	if descriptor.Type != 0 || len(descriptor.Descriptor) < 2 {
		return 0, ErrRejected
	}

	return uint16(descriptor.Descriptor[0])<<8 | uint16(descriptor.Descriptor[1]), nil
}

// ReadProperty reads count elements of a property, starting at index start.
func (c *Client) ReadProperty(object, propertyID uint8, start uint16, count uint8) ([]byte, error) {
	ref := apdu.PropertyRef{Object: object, PropertyID: propertyID, Count: count, Start: start}

	response, err := c.Request(apdu.PropertyValueRead{PropertyRef: ref}, func(a apdu.APDU) bool {
		r, ok := a.(apdu.PropertyValueResponse)
		return ok && r.Object == object && r.PropertyID == propertyID && r.Start == start
	})
	if err != nil {
		return nil, err
	}

	if r := response.(apdu.PropertyValueResponse); r.Count != 0 {
		return r.Data, nil
	}

	return nil, ErrRejected
}

// ReadMemory reads count bytes (at most 63) of device memory.
func (c *Client) ReadMemory(address uint16, count uint8) ([]byte, error) {
	response, err := c.Request(apdu.MemoryRead{Count: count, Address: address}, func(a apdu.APDU) bool {
		r, ok := a.(apdu.MemoryResponse)
		return ok && r.Address == address
	})
	if err != nil {
		return nil, err
	}

	if data := response.(apdu.MemoryResponse).Data; len(data) != 0 {
		return data, nil
	}

	return nil, ErrRejected
}

// exec runs f on the client's goroutine and waits for it. It returns false if the client stopped.
func (c *Client) exec(f func(now time.Time)) bool {
	cmd := command{run: f, done: make(chan struct{})}

	select {
	case c.commands <- cmd:
	case <-c.stopped:
		return false
	}

	select {
	case <-cmd.done:
		return true
	case <-c.stopped:
		return false
	}
}

// nextEvent waits for the next event of the current call.
func (c *Client) nextEvent(timeout <-chan time.Time) (Event, bool) {
	select {
	case event := <-c.events:
		return event, true
	case <-timeout:
		return Event{}, false
	case <-c.stopped:
		return Event{}, false
	}
}

func (c *Client) discardEvents() {
	for {
		select {
		case <-c.events:
		default:
			return
		}
	}
}

func (c *Client) run() {
	defer close(c.stopped)

	timer := time.NewTimer(time.Hour)
	defer timer.Stop()

	for {
		c.dispatch()

		var tick <-chan time.Time
		if deadline, ok := c.conn.Deadline(); ok {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}

			timer.Reset(time.Until(deadline))
			tick = timer.C
		}

		select {
		case <-c.done:
			c.dispatch()
			return

		case cmd := <-c.commands:
			cmd.run(time.Now())
			c.dispatch()
			close(cmd.done)

		case ind, open := <-c.link.Inbound():
			if !open {
				return
			}

			c.receive(ind)

		case now := <-tick:
			c.conn.Tick(now)
		}
	}
}

// receive passes frames that are addressed to us to the connection.
func (c *Client) receive(ind *cemi.LDataInd) {
	if ind.Control2.IsGroupAddr() {
		return
	}

	// Without our address, the peer's frames to other devices cannot be told apart from frames
	// to us; most links know it after the first confirmed frame.
	if addresser, ok := c.link.(link.Addresser); ok {
		if self := addresser.Address(); self != 0 && cemi.IndividualAddr(ind.Destination) != self {
			return
		}
	}

	c.conn.HandleFrame(time.Now(), ind.Source, ind.Data)
}

// dispatch sends the frames the connection asks for and hands other events to the current call.
func (c *Client) dispatch() {
	for {
		event, ok := c.conn.NextEvent()
		if !ok {
			return
		}

		if event.Kind != EventSend {
			select {
			case c.events <- event:
			default: // nobody is waiting
			}

			continue
		}

		err := c.link.Send(&cemi.LDataReq{LData: cemi.LData{
			Control1:    cemi.Control1Request(event.Priority),
			Control2:    cemi.Control2Hops(6),
			Destination: uint16(event.Peer),
			Data:        event.TPDU,
		}})

		c.conn.HandleConfirm(time.Now(), event.TPDU, err == nil)
	}
}
