// Package transport implements KNX transport layer connections (T_Connect, T_Data_Connected,
// T_ACK, T_NAK, T_Disconnect) as specified in KNX 03_03_04.
//
// Conn is a state machine without I/O: it is fed with received frames, link confirmations and the
// current time, and produces events, including the frames to send. This keeps it deterministic
// and testable, and lets the caller decide how to run it (a goroutine with a timer, a main loop on
// a microcontroller). Client runs a Conn on top of a link.Link.
package transport

import (
	"errors"
	"time"

	"github.com/shabunin/tiny-knx-go/cemi"
)

// ErrNotConnected is returned when data is sent without an open connection.
var ErrNotConnected = errors.New("transport: not connected")

// State is the state of a transport connection.
type State uint8

// These are the connection states.
const (
	Closed State = iota
	OpenIdle
	OpenWait // waiting for the T_ACK of sent data
	Connecting
)

func (s State) String() string {
	switch s {
	case Closed:
		return "Closed"
	case OpenIdle:
		return "OpenIdle"
	case OpenWait:
		return "OpenWait"
	case Connecting:
		return "Connecting"
	}

	return "invalid"
}

// Config configures a connection.
type Config struct {
	// ConnectionTimeout closes the connection when nothing happens for this long.
	ConnectionTimeout time.Duration

	// AckTimeout is how long to wait for a T_ACK before sending the data again.
	AckTimeout time.Duration

	// MaxRepetitions is how often unacknowledged data is sent again.
	MaxRepetitions int

	// DataPriority is the priority of T_Data_Connected frames. Control frames use system
	// priority.
	DataPriority cemi.Priority
}

// DefaultConfig contains the timings from the KNX specification.
var DefaultConfig = Config{
	ConnectionTimeout: 6 * time.Second,
	AckTimeout:        3 * time.Second,
	MaxRepetitions:    3,
	DataPriority:      cemi.PrioLow,
}

// EventKind tells what an Event means.
type EventKind uint8

// These are the event kinds.
const (
	// EventSend asks the caller to send TPDU to Peer.
	EventSend EventKind = iota

	// EventConnected reports an open connection: Peer connected to us, or our T_Connect was
	// confirmed by the bus.
	EventConnected

	// EventDisconnected reports that the connection was closed, by either side or by a timeout.
	// Data that was not confirmed yet is discarded.
	EventDisconnected

	// EventData delivers received data. TPDU is the *cemi.AppData.
	EventData

	// EventConfirmed reports that Peer acknowledged the oldest unconfirmed data.
	EventConfirmed
)

func (k EventKind) String() string {
	switch k {
	case EventSend:
		return "Send"
	case EventConnected:
		return "Connected"
	case EventDisconnected:
		return "Disconnected"
	case EventData:
		return "Data"
	case EventConfirmed:
		return "Confirmed"
	}

	return "invalid"
}

// An Event is an output of a Conn.
type Event struct {
	Kind     EventKind
	Peer     cemi.IndividualAddr
	Priority cemi.Priority      // for EventSend
	TPDU     cemi.TransportUnit // for EventSend and EventData
}

// Control commands of a T-PDU without data.
const (
	tConnect    = 0
	tDisconnect = 1
	tAck        = 2
	tNak        = 3
)

// A Conn is one end of a transport connection. The comments refer to the events (E0–E26) and
// actions (A0–A15) of the state machine in the KNX specification.
type Conn struct {
	config Config
	state  State
	peer   cemi.IndividualAddr

	seqSend uint8
	seqRecv uint8
	reps    int

	pending *cemi.AppData   // sent and not yet acknowledged
	queue   []*cemi.AppData // waiting until the connection is idle

	connTimer    bool
	connDeadline time.Time
	ackTimer     bool
	ackDeadline  time.Time

	events []Event
	next   int // index of the next event to return
}

// NewConn returns a closed connection.
func NewConn(config Config) *Conn {
	if config.ConnectionTimeout <= 0 {
		config.ConnectionTimeout = DefaultConfig.ConnectionTimeout
	}

	if config.AckTimeout <= 0 {
		config.AckTimeout = DefaultConfig.AckTimeout
	}

	if config.MaxRepetitions <= 0 {
		config.MaxRepetitions = DefaultConfig.MaxRepetitions
	}

	return &Conn{config: config}
}

// State returns the current state.
func (c *Conn) State() State {
	return c.state
}

// Peer returns the address of the remote end of the current or last connection.
func (c *Conn) Peer() cemi.IndividualAddr {
	return c.peer
}

// NextEvent returns the next pending event.
func (c *Conn) NextEvent() (Event, bool) {
	if c.next == len(c.events) {
		c.events = c.events[:0]
		c.next = 0

		return Event{}, false
	}

	event := c.events[c.next]
	c.events[c.next] = Event{}
	c.next++

	return event, true
}

// Deadline returns when Tick must be called next, if a timer is running.
func (c *Conn) Deadline() (time.Time, bool) {
	switch {
	case c.connTimer && c.ackTimer:
		if c.ackDeadline.Before(c.connDeadline) {
			return c.ackDeadline, true
		}

		return c.connDeadline, true

	case c.connTimer:
		return c.connDeadline, true

	case c.ackTimer:
		return c.ackDeadline, true
	}

	return time.Time{}, false
}

// HandleFrame processes a frame that was received from src and addressed to us. It returns false
// if the frame does not belong to connection-oriented communication (group or connectionless
// data), and leaves such frames to the caller.
func (c *Conn) HandleFrame(now time.Time, src cemi.IndividualAddr, tpdu cemi.TransportUnit) bool {
	switch tpdu := tpdu.(type) {
	case *cemi.ControlData:
		switch tpdu.Command {
		case tConnect:
			c.handleConnect(now, src)
		case tDisconnect:
			c.handleDisconnect(src)
		case tAck:
			c.handleAck(now, src, tpdu.SeqNumber)
		case tNak:
			c.handleNak(now, src, tpdu.SeqNumber)
		}

		return true

	case *cemi.AppData:
		if !tpdu.Numbered {
			return false
		}

		c.handleData(now, src, tpdu)

		return true
	}

	return false
}

func (c *Conn) handleConnect(now time.Time, src cemi.IndividualAddr) {
	switch c.state {
	case Closed: // E0, E1
		c.state = OpenIdle
		c.a1(now, src)

	default:
		if src != c.peer { // E1
			c.a10(src)
		} // E0: A0
	}
}

func (c *Conn) handleDisconnect(src cemi.IndividualAddr) {
	if src == c.peer && c.state != Closed { // E2
		c.state = Closed
		c.a5()
	} // E2 in Closed, E3: A0
}

func (c *Conn) handleData(now time.Time, src cemi.IndividualAddr, app *cemi.AppData) {
	if src != c.peer { // E7
		if c.state == Connecting {
			c.a10(src)
		}

		return
	}

	seq := app.SeqNumber & 15

	switch {
	case seq == c.seqRecv: // E4
		switch c.state {
		case OpenIdle, OpenWait:
			c.a2(now, app)
		case Connecting:
			c.state = Closed
			c.a6()
		}

	case seq == (c.seqRecv-1)&15: // E5
		if c.state != Closed {
			c.a3(now, seq)
		}

	default: // E6
		switch c.state {
		case OpenIdle, OpenWait:
			c.a4(now, seq)
		case Connecting:
			c.state = Closed
			c.a6()
		}
	}
}

func (c *Conn) handleAck(now time.Time, src cemi.IndividualAddr, seq uint8) {
	if src != c.peer { // E10
		if c.state == Connecting {
			c.a10(src)
		}

		return
	}

	if seq&15 == c.seqSend { // E8
		switch c.state {
		case OpenWait:
			c.state = OpenIdle
			c.a8(now)
		case Connecting:
			c.state = Closed
			c.a6()
		}

		return
	}

	// E9
	if c.state == OpenWait || c.state == Connecting {
		c.state = Closed
		c.a6()
	}
}

func (c *Conn) handleNak(now time.Time, src cemi.IndividualAddr, seq uint8) {
	if src != c.peer { // E14
		if c.state == Connecting {
			c.a10(src)
		}

		return
	}

	if seq&15 != c.seqSend { // E11
		if c.state == Connecting {
			c.state = Closed
			c.a6()
		}

		return
	}

	if c.reps < c.config.MaxRepetitions { // E12
		switch c.state {
		case OpenWait:
			c.a9(now)
		case OpenIdle, Connecting:
			c.state = Closed
			c.a6()
		}

		return
	}

	// E13
	if c.state != Closed {
		c.state = Closed
		c.a6()
	}
}

// HandleConfirm processes the link layer confirmation of a frame that was sent for an EventSend.
// Only the confirmation of T_Connect matters: a negative one means that the peer does not exist.
func (c *Conn) HandleConfirm(now time.Time, tpdu cemi.TransportUnit, ok bool) {
	control, isControl := tpdu.(*cemi.ControlData)
	if !isControl || control.Command != tConnect || c.state != Connecting {
		return // E21–E24, or E19/E20 outside Connecting: A0
	}

	if ok { // E19
		c.state = OpenIdle
		c.emit(Event{Kind: EventConnected, Peer: c.peer}) // A13
		c.sendQueued(now)
	} else { // E20
		c.state = Closed
		c.a5()
	}
}

// Connect opens a connection to peer. The connection is open after EventConnected.
func (c *Conn) Connect(now time.Time, peer cemi.IndividualAddr) {
	if c.state != Closed { // E25
		c.state = Closed
		c.a6()

		return
	}

	c.state = Connecting

	// A12
	c.peer = peer
	c.seqSend = 0
	c.seqRecv = 0
	c.sendControl(peer, tConnect, 0)
	c.startConnTimer(now)
}

// Disconnect closes the connection.
func (c *Conn) Disconnect() {
	if c.state == Closed { // E26: A15
		return
	}

	c.state = Closed

	// A14
	c.sendControl(c.peer, tDisconnect, 0)
	c.emit(Event{Kind: EventDisconnected, Peer: c.peer})
	c.stop()
}

// Send transmits data over the connection. The data must be the encoding of an APDU (see
// apdu.Encode); its sequence number is set by the connection. Data sent while earlier data is
// unconfirmed, or before the connection is open, is queued.
func (c *Conn) Send(now time.Time, app *cemi.AppData) error {
	switch c.state {
	case Closed: // E15: A0
		return ErrNotConnected

	case OpenIdle:
		c.state = OpenWait
		c.a7(now, app)

	default: // A11
		c.queue = append(c.queue, app)
	}

	return nil
}

// Tick processes timeouts. Call it at or after the time returned by Deadline.
func (c *Conn) Tick(now time.Time) {
	if c.connTimer && !now.Before(c.connDeadline) && c.state != Closed { // E16
		c.state = Closed
		c.a6()

		return
	}

	if c.ackTimer && !now.Before(c.ackDeadline) && c.state == OpenWait {
		if c.reps < c.config.MaxRepetitions { // E17
			c.a9(now)
		} else { // E18
			c.state = Closed
			c.a6()
		}
	}
}

// a1 accepts a connection from src.
func (c *Conn) a1(now time.Time, src cemi.IndividualAddr) {
	c.peer = src
	c.seqSend = 0
	c.seqRecv = 0
	c.emit(Event{Kind: EventConnected, Peer: src})
	c.startConnTimer(now)
}

// a2 acknowledges and delivers expected data.
func (c *Conn) a2(now time.Time, app *cemi.AppData) {
	c.sendControl(c.peer, tAck, c.seqRecv)
	c.seqRecv = (c.seqRecv + 1) & 15
	c.emit(Event{Kind: EventData, Peer: c.peer, TPDU: app})
	c.startConnTimer(now)
}

// a3 acknowledges a repetition of the last data without delivering it again.
func (c *Conn) a3(now time.Time, seq uint8) {
	c.sendControl(c.peer, tAck, seq)
	c.startConnTimer(now)
}

// a4 rejects data with an unexpected sequence number.
func (c *Conn) a4(now time.Time, seq uint8) {
	c.sendControl(c.peer, tNak, seq)
	c.startConnTimer(now)
}

// a5 reports a closed connection.
func (c *Conn) a5() {
	c.emit(Event{Kind: EventDisconnected, Peer: c.peer})
	c.stop()
}

// a6 closes the connection and tells the peer.
func (c *Conn) a6() {
	c.sendControl(c.peer, tDisconnect, 0)
	c.a5()
}

// a7 sends data.
func (c *Conn) a7(now time.Time, app *cemi.AppData) {
	numbered := *app
	numbered.Numbered = true
	numbered.SeqNumber = c.seqSend
	c.pending = &numbered

	c.emit(Event{Kind: EventSend, Peer: c.peer, Priority: c.config.DataPriority, TPDU: c.pending})
	c.reps = 0
	c.startAckTimer(now)
	c.startConnTimer(now)
}

// a8 completes sent data after its acknowledgement.
func (c *Conn) a8(now time.Time) {
	c.ackTimer = false
	c.pending = nil
	c.seqSend = (c.seqSend + 1) & 15
	c.emit(Event{Kind: EventConfirmed, Peer: c.peer})
	c.startConnTimer(now)
	c.sendQueued(now)
}

// a9 repeats unacknowledged data.
func (c *Conn) a9(now time.Time) {
	c.emit(Event{Kind: EventSend, Peer: c.peer, Priority: c.config.DataPriority, TPDU: c.pending})
	c.reps++
	c.startAckTimer(now)
	c.startConnTimer(now)
}

// a10 rejects a connection attempt or frame from a device that is not our peer.
func (c *Conn) a10(src cemi.IndividualAddr) {
	c.sendControl(src, tDisconnect, 0)
}

// sendQueued sends the next queued data once the connection is idle.
func (c *Conn) sendQueued(now time.Time) {
	if c.state != OpenIdle || len(c.queue) == 0 {
		return
	}

	next := c.queue[0]
	c.queue = c.queue[1:]
	c.state = OpenWait
	c.a7(now, next)
}

func (c *Conn) sendControl(dest cemi.IndividualAddr, command uint8, seq uint8) {
	control := &cemi.ControlData{Command: command}
	if command == tAck || command == tNak {
		control.Numbered = true
		control.SeqNumber = seq
	}

	c.emit(Event{Kind: EventSend, Peer: dest, Priority: cemi.PrioSystem, TPDU: control})
}

func (c *Conn) emit(event Event) {
	c.events = append(c.events, event)
}

func (c *Conn) startConnTimer(now time.Time) {
	c.connTimer = true
	c.connDeadline = now.Add(c.config.ConnectionTimeout)
}

func (c *Conn) startAckTimer(now time.Time) {
	c.ackTimer = true
	c.ackDeadline = now.Add(c.config.AckTimeout)
}

// stop ends all timers and discards unsent data.
func (c *Conn) stop() {
	c.connTimer = false
	c.ackTimer = false
	c.pending = nil
	c.queue = nil
}
