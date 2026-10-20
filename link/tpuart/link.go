// Package tpuart connects to a KNX TP1 bus through a TP-UART transceiver: Siemens TP-UART2 or
// onsemi NCN5120/NCN5121/NCN5130 in UART mode (19200 baud, 8 data bits, even parity, 1 stop bit).
//
// Unlike a BAOS module, the transceiver leaves the data link layer to the host: the Link converts
// between cEMI and TP1 frames, tells the chip which frames to acknowledge on the bus, and filters
// the echo of its own frames.
package tpuart

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shabunin/tiny-knx-go/cemi"
	"github.com/shabunin/tiny-knx-go/link"
)

// ErrNoResponse is returned by Open when the chip does not answer the reset request.
var ErrNoResponse = errors.New("tpuart: no reset indication from the chip")

// Chip selects the transceiver, where they differ.
type Chip uint8

const (
	// ChipNCN5120 covers the NCN5120, NCN5121 and NCN5130.
	ChipNCN5120 Chip = iota

	// ChipTPUART2 is the Siemens TP-UART2.
	ChipTPUART2
)

// Config configures a Link.
type Config struct {
	Chip Chip

	// Address is the individual address that the link sends with and acknowledges. Zero means
	// 15.15.255, the address of an unprogrammed device.
	Address cemi.IndividualAddr

	// AcceptGroup decides which group telegrams are acknowledged and delivered. Nil accepts all.
	// Broadcasts (group address 0/0/0) are always accepted.
	AcceptGroup func(cemi.GroupAddr) bool

	// AllFrames delivers frames for other individual addresses and rejected group addresses too,
	// e.g. for a bus monitor. They are still not acknowledged.
	AllFrames bool

	// ResetTimeout is how long to wait for the chip to answer a reset request.
	ResetTimeout time.Duration

	// ConfirmTimeout is how long to wait for the L_Data.con of a sent frame. The chip confirms
	// after the frame went out on the bus, including repetitions.
	ConfirmTimeout time.Duration

	// FrameTimeout is the pause within a frame after which it is broken off. USB serial adapters
	// deliver bytes in bursts, so it must be longer than their latency.
	FrameTimeout time.Duration

	// ResyncGap is the pause that ends the discarding of bytes after a broken frame or an unknown
	// byte.
	ResyncGap time.Duration

	// InboundQueue is the number of received frames that are buffered for the reader. When the
	// queue is full, new frames are dropped (see Link.Dropped).
	InboundQueue int
}

// DefaultConfig works with an NCN5120 on a direct UART connection.
var DefaultConfig = Config{
	Chip:           ChipNCN5120,
	ResetTimeout:   500 * time.Millisecond,
	ConfirmTimeout: 3 * time.Second,
	FrameTimeout:   50 * time.Millisecond,
	ResyncGap:      4 * time.Millisecond,
	InboundQueue:   32,
}

func (cfg Config) withDefaults() Config {
	if cfg.Address == 0 {
		cfg.Address = cemi.NewIndividualAddr3(15, 15, 255)
	}

	if cfg.ResetTimeout <= 0 {
		cfg.ResetTimeout = DefaultConfig.ResetTimeout
	}

	if cfg.ConfirmTimeout <= 0 {
		cfg.ConfirmTimeout = DefaultConfig.ConfirmTimeout
	}

	if cfg.FrameTimeout <= 0 {
		cfg.FrameTimeout = DefaultConfig.FrameTimeout
	}

	if cfg.ResyncGap <= 0 {
		cfg.ResyncGap = DefaultConfig.ResyncGap
	}

	if cfg.InboundQueue <= 0 {
		cfg.InboundQueue = DefaultConfig.InboundQueue
	}

	return cfg
}

// A Link exchanges frames with a TP1 bus through a TP-UART or NCN5120.
//
// The port must deliver bytes as they arrive. A Read that returns no data and no error (as
// TinyGo's machine.UART does) is retried after a short pause.
type Link struct {
	port   io.ReadWriter
	config Config

	writeMu sync.Mutex // serialises writes to the port
	sendMu  sync.Mutex // one frame awaiting confirmation at a time

	mu sync.Mutex
	tx []byte // the frame awaiting confirmation

	address  atomic.Uint32
	state    atomic.Uint32 // flags of the last U_State.ind
	dropped  atomic.Uint32
	starting atomic.Bool // Open is waiting for the reset indication

	cons    chan bool
	resets  chan struct{}
	inbound chan *cemi.LDataInd

	done chan struct{}
	once sync.Once

	// Used by the serve goroutine only.
	rx        *receiver
	echo      bool   // the current frame is our own
	addressed bool   // the current frame was accepted
	last      []byte // the last delivered frame, to drop its repetitions
}

var (
	_ link.Link          = (*Link)(nil)
	_ link.Addresser     = (*Link)(nil)
	_ link.AddressSetter = (*Link)(nil)
)

// Open resets the chip behind port and sets its individual address.
func Open(port io.ReadWriter, config Config) (*Link, error) {
	config = config.withDefaults()

	l := &Link{
		port:    port,
		config:  config,
		cons:    make(chan bool, 1),
		resets:  make(chan struct{}, 1),
		inbound: make(chan *cemi.LDataInd, config.InboundQueue),
		done:    make(chan struct{}),
		rx:      newReceiver(config.FrameTimeout, config.ResyncGap),
	}

	l.address.Store(uint32(config.Address))

	go l.serve()

	if err := l.init(); err != nil {
		l.Close()
		return nil, err
	}

	return l, nil
}

func (l *Link) init() error {
	l.starting.Store(true)
	defer l.starting.Store(false)

	for attempt := 0; attempt < 3; attempt++ {
		select {
		case <-l.resets:
		default:
		}

		if err := l.write([]byte{ResetReq}); err != nil {
			return err
		}

		timer := time.NewTimer(l.config.ResetTimeout)

		select {
		case <-l.resets:
			timer.Stop()
			return l.configure()

		case <-timer.C:

		case <-l.done:
			timer.Stop()
			return link.ErrClosed
		}
	}

	return ErrNoResponse
}

// configure sends the settings that the chip forgets when it resets.
func (l *Link) configure() error {
	if err := l.writeAddress(l.Address()); err != nil {
		return err
	}

	return l.write([]byte{StateReq})
}

func (l *Link) writeAddress(addr cemi.IndividualAddr) error {
	request := []byte{SetAddressReq, byte(addr >> 8), byte(addr)}
	if l.config.Chip == ChipNCN5120 {
		request = append(request, 0xFF) // dummy byte
	}

	return l.write(request)
}

// Send transmits the frame and waits for its L_Data.con. A source address of 0.0.0 is replaced by
// the link's address.
func (l *Link) Send(req *cemi.LDataReq) error {
	ldata := req.LData
	if ldata.Source == 0 {
		ldata.Source = l.Address()
	}

	frame, err := AppendFrame(nil, &ldata)
	if err != nil {
		return err
	}

	l.sendMu.Lock()
	defer l.sendMu.Unlock()

	// Discard a confirmation that arrived after its request timed out.
	select {
	case <-l.cons:
	default:
	}

	l.mu.Lock()
	l.tx = frame
	l.mu.Unlock()

	defer func() {
		l.mu.Lock()
		l.tx = nil
		l.mu.Unlock()
	}()

	if err := l.write(appendSendRequest(nil, frame)); err != nil {
		return err
	}

	timeout := time.NewTimer(l.config.ConfirmTimeout)
	defer timeout.Stop()

	select {
	case ok := <-l.cons:
		if !ok {
			return link.ErrNotConfirmed
		}

		return nil

	case <-timeout.C:
		return link.ErrConfirmTimeout

	case <-l.done:
		return link.ErrClosed
	}
}

// Address returns the individual address of the link.
func (l *Link) Address() cemi.IndividualAddr {
	return cemi.IndividualAddr(l.address.Load())
}

// SetAddress changes the individual address that the link sends with and acknowledges.
func (l *Link) SetAddress(addr cemi.IndividualAddr) error {
	l.address.Store(uint32(addr))

	return l.writeAddress(addr)
}

// State returns the error flags of the last U_State.ind (State* constants).
func (l *Link) State() uint8 {
	return uint8(l.state.Load())
}

// Inbound returns the frames received from the bus.
func (l *Link) Inbound() <-chan *cemi.LDataInd {
	return l.inbound
}

// Dropped returns the number of received frames that were dropped because the inbound queue was
// full.
func (l *Link) Dropped() uint32 {
	return l.dropped.Load()
}

// Close terminates the link and closes the port if it implements io.Closer.
func (l *Link) Close() error {
	var err error

	l.once.Do(func() {
		close(l.done)

		if closer, ok := l.port.(io.Closer); ok {
			err = closer.Close()
		}
	})

	return err
}

func (l *Link) write(data []byte) error {
	l.writeMu.Lock()
	defer l.writeMu.Unlock()

	select {
	case <-l.done:
		return link.ErrClosed
	default:
	}

	_, err := l.port.Write(data)

	return err
}

// sending returns the frame awaiting confirmation, if any.
func (l *Link) sending() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.tx
}

func (l *Link) serve() {
	defer close(l.inbound)

	buffer := make([]byte, 64)

	for {
		n, err := l.port.Read(buffer)

		select {
		case <-l.done:
			return
		default:
		}

		if err != nil {
			l.Close()
			return
		}

		if n == 0 {
			time.Sleep(time.Millisecond)
			continue
		}

		now := time.Now()

		for _, b := range buffer[:n] {
			l.handle(b, now)
		}
	}
}

func (l *Link) handle(b byte, now time.Time) {
	switch l.rx.feed(b, now) {
	case rxFrameStart:
		l.echo, l.addressed = false, false

		// Clear an acknowledgement that the chip may still hold from the previous frame. We
		// never acknowledge while sending, so as not to acknowledge our own frame.
		if l.sending() == nil {
			l.write([]byte{AckInfoReq})
		}

	case rxHeader:
		head := l.rx.frame
		tx := l.sending()

		l.echo = tx != nil && sameHeader(head, tx)
		l.addressed = !l.echo && l.accepts(head)

		if l.addressed && tx == nil {
			l.write([]byte{AckInfoReq | AckInfoAddressed})
		}

	case rxFrame:
		if !l.echo && (l.addressed || l.config.AllFrames) {
			l.deliver(l.rx.frame)
		}

	case rxReset:
		select {
		case l.resets <- struct{}{}:
		default:
		}

		// The chip restarted on its own and forgot its address.
		if !l.starting.Load() {
			go l.configure()
		}

	case rxStateInd:
		l.state.Store(uint32(l.rx.value &^ stateIndMask))

	case rxConfirm:
		if l.sending() != nil {
			select {
			case l.cons <- l.rx.value != 0:
			default:
			}
		}
	}
}

// accepts tells whether a frame with the given header is for us.
func (l *Link) accepts(head []byte) bool {
	_, dest, group := frameAddresses(head)

	if !group {
		return cemi.IndividualAddr(dest) == l.Address()
	}

	return dest == 0 || l.config.AcceptGroup == nil || l.config.AcceptGroup(cemi.GroupAddr(dest))
}

// sameHeader tells whether a received frame header belongs to the frame we are sending. The chip
// may have repeated it, which clears the repeat flag.
func sameHeader(head, tx []byte) bool {
	if (head[0]^tx[0])&^ctrlNotRepeat != 0 {
		return false
	}

	src, dest, group := frameAddresses(head)
	txSrc, txDest, txGroup := frameAddresses(tx)

	return src == txSrc && dest == txDest && group == txGroup
}

// deliver passes a frame to the reader, unless it repeats the previous one.
func (l *Link) deliver(frame []byte) {
	body := frame[1 : len(frame)-1] // without control byte and checksum

	if frame[0]&ctrlNotRepeat == 0 && l.last != nil &&
		frame[0]|ctrlNotRepeat == l.last[0] && bytes.Equal(body, l.last[1:]) {
		return
	}

	l.last = append(l.last[:0], frame[0]|ctrlNotRepeat)
	l.last = append(l.last, body...)

	ldata, err := ParseFrame(frame)
	if err != nil {
		return
	}

	select {
	case l.inbound <- &cemi.LDataInd{LData: ldata}:
	default:
		l.dropped.Add(1)
	}
}
