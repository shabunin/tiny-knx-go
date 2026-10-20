package transport

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/shabunin/tiny-knx-go/cemi"
)

var (
	clientAddr = cemi.NewIndividualAddr3(1, 1, 240)
	deviceAddr = cemi.NewIndividualAddr3(1, 1, 3)
	otherAddr  = cemi.NewIndividualAddr3(1, 1, 7)
)

// wire sends a TPDU through the cEMI encoding, as it would travel over a real bus.
func wire(t *testing.T, tpdu cemi.TransportUnit) cemi.TransportUnit {
	t.Helper()

	frame := &cemi.LDataInd{LData: cemi.LData{Data: tpdu}}
	buffer := make([]byte, cemi.Size(frame))
	cemi.Pack(buffer, frame)

	var msg cemi.Message
	if _, err := cemi.Unpack(buffer, &msg); err != nil {
		t.Fatalf("Unpack: %v", err)
	}

	return msg.(*cemi.LDataInd).Data
}

func describe(tpdu cemi.TransportUnit) string {
	switch tpdu := tpdu.(type) {
	case *cemi.ControlData:
		switch tpdu.Command {
		case tConnect:
			return "T_Connect"
		case tDisconnect:
			return "T_Disconnect"
		case tAck:
			return fmt.Sprintf("T_ACK %d", tpdu.SeqNumber)
		case tNak:
			return fmt.Sprintf("T_NAK %d", tpdu.SeqNumber)
		}

	case *cemi.AppData:
		return fmt.Sprintf("T_Data %d % x", tpdu.SeqNumber, tpdu.Data)
	}

	return "?"
}

type endpoint struct {
	name   string
	addr   cemi.IndividualAddr
	conn   *Conn
	events []Event // all events except EventSend
}

func (e *endpoint) kinds() string {
	var kinds []string
	for _, event := range e.events {
		kinds = append(kinds, event.Kind.String())
	}

	return strings.Join(kinds, ",")
}

func (e *endpoint) data() []string {
	var data []string
	for _, event := range e.events {
		if event.Kind == EventData {
			data = append(data, fmt.Sprintf("% x", event.TPDU.(*cemi.AppData).Data))
		}
	}

	return data
}

// pair connects a client and a device through a simulated bus with a virtual clock.
type pair struct {
	t      *testing.T
	now    time.Time
	client *endpoint
	device *endpoint
	drop   func(from *endpoint, tpdu cemi.TransportUnit) bool
	log    []string
}

func newPair(t *testing.T) *pair {
	return &pair{
		t:      t,
		now:    time.Unix(0, 0),
		client: &endpoint{name: "client", addr: clientAddr, conn: NewConn(DefaultConfig)},
		device: &endpoint{name: "device", addr: deviceAddr, conn: NewConn(DefaultConfig)},
	}
}

// pump moves frames between the two ends until both are quiet.
func (p *pair) pump() {
	for moved := true; moved; {
		moved = false

		for _, from := range []*endpoint{p.client, p.device} {
			to := p.device
			if from == p.device {
				to = p.client
			}

			for {
				event, ok := from.conn.NextEvent()
				if !ok {
					break
				}

				moved = true

				if event.Kind != EventSend {
					from.events = append(from.events, event)
					continue
				}

				// Only the two ends exist on this bus.
				reachable := event.Peer == to.addr
				from.conn.HandleConfirm(p.now, event.TPDU, reachable)

				if !reachable {
					p.log = append(p.log, fmt.Sprintf("%s->%v %s (unconfirmed)", from.name, event.Peer, describe(event.TPDU)))
					continue
				}

				line := fmt.Sprintf("%s->%s %s", from.name, to.name, describe(event.TPDU))
				if p.drop != nil && p.drop(from, event.TPDU) {
					p.log = append(p.log, line+" (lost)")
					continue
				}

				p.log = append(p.log, line)
				to.conn.HandleFrame(p.now, from.addr, wire(p.t, event.TPDU))
			}
		}
	}
}

// advance moves the clock forward and runs the timers.
func (p *pair) advance(d time.Duration) {
	p.now = p.now.Add(d)
	p.client.conn.Tick(p.now)
	p.device.conn.Tick(p.now)
	p.pump()
}

func (p *pair) connect() {
	p.client.conn.Connect(p.now, deviceAddr)
	p.pump()

	if p.client.conn.State() != OpenIdle || p.device.conn.State() != OpenIdle {
		p.t.Fatalf("Not connected: client %v, device %v", p.client.conn.State(), p.device.conn.State())
	}

	p.log = nil
	p.client.events = nil
	p.device.events = nil
}

func (p *pair) send(from *endpoint, data ...byte) {
	p.t.Helper()

	if err := from.conn.Send(p.now, &cemi.AppData{Command: cemi.MaskVersionRead, Data: data}); err != nil {
		p.t.Fatalf("Send: %v", err)
	}

	p.pump()
}

func (p *pair) expectLog(expected ...string) {
	p.t.Helper()

	if strings.Join(p.log, "\n") != strings.Join(expected, "\n") {
		p.t.Errorf("Bus traffic:\n  %s\nexpected:\n  %s", strings.Join(p.log, "\n  "), strings.Join(expected, "\n  "))
	}

	p.log = nil
}

func TestConnectRequestResponseDisconnect(t *testing.T) {
	p := newPair(t)

	p.client.conn.Connect(p.now, deviceAddr)
	p.pump()

	p.send(p.client, 0x00)       // DeviceDescriptorRead
	p.send(p.device, 0x40, 7, 1) // DeviceDescriptorResponse

	p.client.conn.Disconnect()
	p.pump()

	p.expectLog(
		"client->device T_Connect",
		"client->device T_Data 0 00",
		"device->client T_ACK 0",
		"device->client T_Data 0 40 07 01",
		"client->device T_ACK 0",
		"client->device T_Disconnect",
	)

	if got := p.client.kinds(); got != "Connected,Confirmed,Data,Disconnected" {
		t.Errorf("Client events %s", got)
	}

	if got := p.device.kinds(); got != "Connected,Data,Confirmed,Disconnected" {
		t.Errorf("Device events %s", got)
	}

	if p.client.conn.State() != Closed || p.device.conn.State() != Closed {
		t.Errorf("States after disconnect: client %v, device %v", p.client.conn.State(), p.device.conn.State())
	}
}

func TestSequenceNumbersWrap(t *testing.T) {
	p := newPair(t)
	p.connect()

	for i := 0; i < 20; i++ {
		p.send(p.client, byte(i))
	}

	data := p.device.data()
	if len(data) != 20 {
		t.Fatalf("Device received %d messages, expected 20", len(data))
	}

	for i, d := range data {
		if d != fmt.Sprintf("%02x", i) {
			t.Errorf("Message %d is %s", i, d)
		}
	}

	if !strings.HasSuffix(p.log[len(p.log)-2], "T_Data 3 13") {
		t.Errorf("Sequence number did not wrap: %s", p.log[len(p.log)-2])
	}
}

func TestLostAckIsRepeated(t *testing.T) {
	p := newPair(t)
	p.connect()

	lost := false
	p.drop = func(from *endpoint, tpdu cemi.TransportUnit) bool {
		if control, ok := tpdu.(*cemi.ControlData); ok && control.Command == tAck && !lost {
			lost = true
			return true
		}

		return false
	}

	p.send(p.client, 0x00)
	p.advance(DefaultConfig.AckTimeout)

	p.expectLog(
		"client->device T_Data 0 00",
		"device->client T_ACK 0 (lost)",
		"client->device T_Data 0 00",
		"device->client T_ACK 0",
	)

	if got := p.device.data(); len(got) != 1 {
		t.Errorf("Device delivered %d messages, expected 1", len(got))
	}

	if got := p.client.kinds(); got != "Confirmed" {
		t.Errorf("Client events %s", got)
	}
}

func TestAckTimeoutDisconnects(t *testing.T) {
	now := time.Unix(0, 0)
	client := NewConn(DefaultConfig)

	client.Connect(now, deviceAddr)
	connect, _ := client.NextEvent()
	client.HandleConfirm(now, connect.TPDU, true)
	client.Send(now, &cemi.AppData{Data: []byte{0}})

	// The peer never answers: the data goes out once plus MaxRepetitions times, then the
	// connection is closed.
	var sent []string
	for i := 0; i <= DefaultConfig.MaxRepetitions; i++ {
		if i > 0 {
			deadline, _ := client.Deadline()
			if deadline != now.Add(DefaultConfig.AckTimeout) {
				t.Fatalf("Next deadline %v, expected the ack timeout", deadline.Sub(now))
			}

			now = deadline
			client.Tick(now)
		}

		for event, ok := client.NextEvent(); ok; event, ok = client.NextEvent() {
			if event.Kind == EventSend {
				sent = append(sent, describe(event.TPDU))
			}
		}
	}

	now = now.Add(DefaultConfig.AckTimeout)
	client.Tick(now)

	for event, ok := client.NextEvent(); ok; event, ok = client.NextEvent() {
		sent = append(sent, event.Kind.String()+" "+describe(event.TPDU))
	}

	expected := []string{
		"T_Data 0 00", "T_Data 0 00", "T_Data 0 00", "T_Data 0 00",
		"Send T_Disconnect", "Disconnected ?",
	}

	if strings.Join(sent, ", ") != strings.Join(expected, ", ") {
		t.Errorf("Got %s, expected %s", strings.Join(sent, ", "), strings.Join(expected, ", "))
	}
}

func TestDeviceTimesOutWhenNothingArrives(t *testing.T) {
	p := newPair(t)
	p.connect()

	p.drop = func(from *endpoint, tpdu cemi.TransportUnit) bool {
		return from == p.client
	}

	p.send(p.client, 0x00)
	p.advance(DefaultConfig.AckTimeout)
	p.advance(DefaultConfig.AckTimeout)

	// 6 s after the connect the device has heard nothing and closes the connection; the client's
	// second repetition is due at the same moment.
	p.expectLog(
		"client->device T_Data 0 00 (lost)",
		"client->device T_Data 0 00 (lost)",
		"client->device T_Data 0 00 (lost)",
		"device->client T_Disconnect",
	)

	if p.client.conn.State() != Closed || p.device.conn.State() != Closed {
		t.Errorf("States: client %v, device %v", p.client.conn.State(), p.device.conn.State())
	}
}

func TestConnectionTimeout(t *testing.T) {
	p := newPair(t)
	p.connect()

	// Activity keeps the connection open.
	p.advance(5 * time.Second)
	p.send(p.client, 0x00)
	p.advance(5 * time.Second)

	if p.device.conn.State() != OpenIdle {
		t.Fatalf("Device state %v, expected OpenIdle", p.device.conn.State())
	}

	p.log = nil

	// Both ends time out after 6 s without traffic and disconnect.
	p.advance(time.Second)

	if p.device.conn.State() != Closed || p.client.conn.State() != Closed {
		t.Errorf("States after timeout: client %v, device %v", p.client.conn.State(), p.device.conn.State())
	}

	if len(p.log) == 0 || !strings.HasSuffix(p.log[0], "T_Disconnect") {
		t.Errorf("Expected a T_Disconnect, got %v", p.log)
	}
}

func TestSecondConnectionIsRejected(t *testing.T) {
	now := time.Unix(0, 0)
	device := NewConn(DefaultConfig)

	device.HandleFrame(now, clientAddr, &cemi.ControlData{Command: tConnect})
	device.NextEvent() // Connected

	device.HandleFrame(now, otherAddr, &cemi.ControlData{Command: tConnect})

	event, ok := device.NextEvent()
	if !ok || event.Kind != EventSend || event.Peer != otherAddr || describe(event.TPDU) != "T_Disconnect" {
		t.Errorf("Expected T_Disconnect to %v, got %+v", otherAddr, event)
	}

	if device.State() != OpenIdle || device.Peer() != clientAddr {
		t.Errorf("Device lost its connection: %v with %v", device.State(), device.Peer())
	}

	// Data from a device that is not connected is ignored.
	device.HandleFrame(now, otherAddr, &cemi.AppData{Numbered: true, Command: cemi.MaskVersionRead, Data: []byte{0}})

	if event, ok := device.NextEvent(); ok {
		t.Errorf("Unexpected event %+v", event)
	}
}

func TestWrongSequenceNumberIsRejected(t *testing.T) {
	now := time.Unix(0, 0)
	device := NewConn(DefaultConfig)

	device.HandleFrame(now, clientAddr, &cemi.ControlData{Command: tConnect})
	device.NextEvent()

	device.HandleFrame(now, clientAddr, &cemi.AppData{Numbered: true, SeqNumber: 5, Data: []byte{0}})

	event, _ := device.NextEvent()
	if event.Kind != EventSend || describe(event.TPDU) != "T_NAK 5" {
		t.Errorf("Expected T_NAK 5, got %+v", event)
	}

	if event, ok := device.NextEvent(); ok {
		t.Errorf("Unexpected event %+v", event)
	}
}

func TestNakRepeatsThenDisconnects(t *testing.T) {
	p := newPair(t)
	p.connect()

	// Replace every T_ACK of the device with a T_NAK.
	p.drop = func(from *endpoint, tpdu cemi.TransportUnit) bool {
		control, ok := tpdu.(*cemi.ControlData)
		if ok && control.Command == tAck {
			p.log = append(p.log, "device->client T_NAK 0")
			p.client.conn.HandleFrame(p.now, deviceAddr, &cemi.ControlData{Numbered: true, Command: tNak})
			return true
		}

		return false
	}

	p.send(p.client, 0x00)

	if got := p.client.kinds(); got != "Disconnected" {
		t.Errorf("Client events %s", got)
	}

	if sent := strings.Count(strings.Join(p.log, "\n"), "client->device T_Data"); sent != DefaultConfig.MaxRepetitions+1 {
		t.Errorf("Data sent %d times, expected %d:\n%s", sent, DefaultConfig.MaxRepetitions+1, strings.Join(p.log, "\n"))
	}
}

func TestConnectToMissingDevice(t *testing.T) {
	p := newPair(t)

	p.client.conn.Connect(p.now, otherAddr)
	p.pump()

	p.expectLog("client->1.1.7 T_Connect (unconfirmed)")

	if p.client.conn.State() != Closed || p.client.kinds() != "Disconnected" {
		t.Errorf("Client %v with events %s", p.client.conn.State(), p.client.kinds())
	}
}

func TestDataIsQueued(t *testing.T) {
	p := newPair(t)

	// Data sent before the connection is confirmed waits, then goes out one at a time.
	p.client.conn.Connect(p.now, deviceAddr)
	for i := byte(1); i <= 3; i++ {
		p.client.conn.Send(p.now, &cemi.AppData{Data: []byte{i}})
	}

	p.pump()

	p.expectLog(
		"client->device T_Connect",
		"client->device T_Data 0 01",
		"device->client T_ACK 0",
		"client->device T_Data 1 02",
		"device->client T_ACK 1",
		"client->device T_Data 2 03",
		"device->client T_ACK 2",
	)
}

func TestSendWhenClosed(t *testing.T) {
	conn := NewConn(DefaultConfig)

	if err := conn.Send(time.Unix(0, 0), &cemi.AppData{}); !errors.Is(err, ErrNotConnected) {
		t.Errorf("Send: got %v, expected %v", err, ErrNotConnected)
	}
}

func TestRemoteDisconnect(t *testing.T) {
	p := newPair(t)
	p.connect()

	p.device.conn.Disconnect()
	p.pump()

	p.expectLog("device->client T_Disconnect")

	if got := p.client.kinds(); got != "Disconnected" || p.client.conn.State() != Closed {
		t.Errorf("Client %v with events %s", p.client.conn.State(), got)
	}

	if _, ok := p.client.conn.Deadline(); ok {
		t.Error("Timers still running after disconnect")
	}
}
