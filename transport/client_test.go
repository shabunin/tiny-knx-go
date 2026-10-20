package transport

import (
	"bytes"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shabunin/tiny-knx-go/apdu"
	"github.com/shabunin/tiny-knx-go/cemi"
	"github.com/shabunin/tiny-knx-go/link"
	"github.com/shabunin/tiny-knx-go/link/mock"
)

var fastConfig = Config{
	ConnectionTimeout: time.Second,
	AckTimeout:        50 * time.Millisecond,
	MaxRepetitions:    3,
}

var serialNumber = []byte{0x00, 0xfa, 0x01, 0x02, 0x03, 0x04}

// simulatedDevice answers management requests over a connection, like a simple KNX device.
type simulatedDevice struct {
	link     *mock.Link
	conn     *Conn
	silent   atomic.Bool // acknowledge requests but never answer
	requests atomic.Int32
	closed   atomic.Bool // the last connection was closed
	stop     chan struct{}
	stopped  chan struct{}
}

func startDevice(t *testing.T, bus *mock.Bus, addr cemi.IndividualAddr) *simulatedDevice {
	d := &simulatedDevice{
		link:    bus.Attach(addr),
		conn:    NewConn(fastConfig),
		stop:    make(chan struct{}),
		stopped: make(chan struct{}),
	}

	go d.run()

	t.Cleanup(func() {
		close(d.stop)
		<-d.stopped
		d.link.Close()
	})

	return d
}

func (d *simulatedDevice) respond(request apdu.APDU) apdu.APDU {
	switch req := request.(type) {
	case apdu.DeviceDescriptorRead:
		return apdu.DeviceDescriptorResponse{Type: 0, Descriptor: []byte{0x07, 0xb0}}

	case apdu.PropertyValueRead:
		if req.Object == 0 && req.PropertyID == 11 {
			return apdu.PropertyValueResponse{PropertyRef: req.PropertyRef, Data: serialNumber}
		}

		ref := req.PropertyRef
		ref.Count = 0

		return apdu.PropertyValueResponse{PropertyRef: ref}

	case apdu.MemoryRead:
		data := make([]byte, req.Count)
		for i := range data {
			data[i] = byte(req.Address) + byte(i)
		}

		return apdu.MemoryResponse{Address: req.Address, Data: data}
	}

	return nil
}

func (d *simulatedDevice) run() {
	defer close(d.stopped)

	for {
		d.process()

		var tick <-chan time.Time
		if deadline, ok := d.conn.Deadline(); ok {
			tick = time.After(time.Until(deadline))
		}

		select {
		case <-d.stop:
			return

		case ind, open := <-d.link.Inbound():
			if !open {
				return
			}

			if !ind.Control2.IsGroupAddr() && cemi.IndividualAddr(ind.Destination) == d.link.Address() {
				d.conn.HandleFrame(time.Now(), ind.Source, ind.Data)
			}

		case now := <-tick:
			d.conn.Tick(now)
		}
	}
}

func (d *simulatedDevice) process() {
	for event, ok := d.conn.NextEvent(); ok; event, ok = d.conn.NextEvent() {
		switch event.Kind {
		case EventSend:
			err := d.link.Send(&cemi.LDataReq{LData: cemi.LData{
				Control1:    cemi.Control1Request(event.Priority),
				Control2:    cemi.Control2Hops(6),
				Destination: uint16(event.Peer),
				Data:        event.TPDU,
			}})
			d.conn.HandleConfirm(time.Now(), event.TPDU, err == nil)

		case EventConnected:
			d.closed.Store(false)

		case EventDisconnected:
			d.closed.Store(true)

		case EventData:
			d.requests.Add(1)

			request, err := apdu.Decode(event.TPDU.(*cemi.AppData))
			if err != nil || d.silent.Load() {
				continue
			}

			if response := d.respond(request); response != nil {
				d.conn.Send(time.Now(), apdu.Encode(response))
			}
		}
	}
}

func newClient(t *testing.T, bus *mock.Bus) *Client {
	l := bus.Attach(clientAddr)
	c := NewClient(l, fastConfig)
	c.ResponseTimeout = 200 * time.Millisecond

	t.Cleanup(func() {
		c.Close()
		l.Close()
	})

	return c
}

func TestClientRequests(t *testing.T) {
	bus := mock.NewBus()
	device := startDevice(t, bus, deviceAddr)
	client := newClient(t, bus)

	if err := client.Connect(deviceAddr); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	if mask, err := client.DeviceDescriptor(); err != nil || mask != 0x07b0 {
		t.Errorf("DeviceDescriptor = %#04x, %v; expected 0x07b0", mask, err)
	}

	if serial, err := client.ReadProperty(0, 11, 1, 1); err != nil || !bytes.Equal(serial, serialNumber) {
		t.Errorf("Serial number % x, %v", serial, err)
	}

	if _, err := client.ReadProperty(0, 200, 1, 1); !errors.Is(err, ErrRejected) {
		t.Errorf("Unknown property: got %v, expected %v", err, ErrRejected)
	}

	if data, err := client.ReadMemory(0x0060, 4); err != nil || !bytes.Equal(data, []byte{0x60, 0x61, 0x62, 0x63}) {
		t.Errorf("Memory % x, %v", data, err)
	}

	client.Disconnect()

	deadline := time.Now().Add(time.Second)
	for !device.closed.Load() {
		if time.Now().After(deadline) {
			t.Fatal("Device did not see the disconnect")
		}

		time.Sleep(time.Millisecond)
	}
}

func TestClientConnectToMissingDevice(t *testing.T) {
	bus := mock.NewBus()
	client := newClient(t, bus)

	if err := client.Connect(deviceAddr); !errors.Is(err, ErrConnectFailed) {
		t.Errorf("Connect: got %v, expected %v", err, ErrConnectFailed)
	}
}

func TestClientNoResponse(t *testing.T) {
	bus := mock.NewBus()
	device := startDevice(t, bus, deviceAddr)
	device.silent.Store(true)
	client := newClient(t, bus)

	if err := client.Connect(deviceAddr); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	if _, err := client.DeviceDescriptor(); !errors.Is(err, ErrNoResponse) {
		t.Errorf("DeviceDescriptor: got %v, expected %v", err, ErrNoResponse)
	}
}

func TestClientLossyBus(t *testing.T) {
	bus := mock.NewBus()
	device := startDevice(t, bus, deviceAddr)
	client := newClient(t, bus)

	// Lose every other frame the device sends: acknowledgements and responses alike.
	var count atomic.Int32
	bus.Drop = func(from cemi.IndividualAddr, frame *cemi.LData) bool {
		return from == deviceAddr && count.Add(1)%2 == 1
	}

	if err := client.Connect(deviceAddr); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	for i := 0; i < 5; i++ {
		if mask, err := client.DeviceDescriptor(); err != nil || mask != 0x07b0 {
			// A lost response is not repeated by the device; the request must be sent again.
			if errors.Is(err, ErrNoResponse) {
				continue
			}

			t.Fatalf("DeviceDescriptor = %#04x, %v", mask, err)
		}
	}

	if device.requests.Load() < 5 {
		t.Errorf("Device handled %d requests, expected at least 5", device.requests.Load())
	}
}

func TestClientIgnoresTrafficToOthers(t *testing.T) {
	bus := mock.NewBus()
	startDevice(t, bus, deviceAddr)
	client := newClient(t, bus)
	other := bus.Attach(otherAddr)

	if err := client.Connect(deviceAddr); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// The device disconnecting some other client must not close our connection. The mock bus
	// shows every frame to every device, like a bus monitor.
	frame := &cemi.LDataReq{LData: cemi.LData{
		Control1:    cemi.Control1Request(cemi.PrioSystem),
		Control2:    cemi.Control2Hops(6),
		Source:      deviceAddr,
		Destination: uint16(otherAddr),
		Data:        &cemi.ControlData{Command: tDisconnect},
	}}

	other.Send(frame)

	if mask, err := client.DeviceDescriptor(); err != nil || mask != 0x07b0 {
		t.Errorf("DeviceDescriptor = %#04x, %v", mask, err)
	}
}

func TestClientClosed(t *testing.T) {
	bus := mock.NewBus()
	l := bus.Attach(clientAddr)
	client := NewClient(l, fastConfig)

	client.Close()

	if err := client.Connect(deviceAddr); !errors.Is(err, link.ErrClosed) {
		t.Errorf("Connect after Close: got %v, expected %v", err, link.ErrClosed)
	}
}
