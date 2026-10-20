package ft12

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"time"
)

var (
	// ErrNoAck is returned when the module did not acknowledge a frame after all retries.
	ErrNoAck = errors.New("ft12: frame not acknowledged")

	// ErrPayloadTooLong is returned for payloads that do not fit into a frame.
	ErrPayloadTooLong = errors.New("ft12: payload too long")

	// ErrClosed is returned after the connection has been closed.
	ErrClosed = errors.New("ft12: connection closed")
)

// ConnConfig configures the FT1.2 connection.
type ConnConfig struct {
	// AckTimeout is how long to wait for the module to acknowledge a frame.
	AckTimeout time.Duration

	// Retries is how often an unacknowledged frame is sent again.
	Retries int

	// InboundQueue is the number of received payloads that are buffered for the reader.
	InboundQueue int
}

// DefaultConnConfig works with Weinzierl BAOS modules.
var DefaultConnConfig = ConnConfig{
	AckTimeout:   500 * time.Millisecond,
	Retries:      3,
	InboundQueue: 16,
}

func (cfg ConnConfig) withDefaults() ConnConfig {
	if cfg.AckTimeout <= 0 {
		cfg.AckTimeout = DefaultConnConfig.AckTimeout
	}

	if cfg.Retries <= 0 {
		cfg.Retries = DefaultConnConfig.Retries
	}

	if cfg.InboundQueue <= 0 {
		cfg.InboundQueue = DefaultConnConfig.InboundQueue
	}

	return cfg
}

// A Conn exchanges payloads with a module over FT1.2: it acknowledges received frames, filters
// retransmissions, and waits for acknowledgements of sent frames, resending them if necessary.
//
// The underlying port must deliver bytes as they arrive. A Read that returns no data and no error
// (as TinyGo's machine.UART does) is retried after a short pause.
type Conn struct {
	port   io.ReadWriter
	config ConnConfig

	writeMu sync.Mutex // serialises writes to the port
	sendMu  sync.Mutex // allows one unacknowledged frame at a time
	fcb     bool       // FCB of the next data frame
	synced  bool       // a reset has been acknowledged since the last failure

	acks    chan struct{}
	resets  chan struct{}
	inbound chan []byte

	recvMu   sync.Mutex
	lastRecv []byte // payload of the last received frame
	lastFCB  byte

	done    chan struct{}
	once    sync.Once
	readErr error
}

// NewConn starts serving the FT1.2 protocol on the given port. Call Reset before sending.
func NewConn(port io.ReadWriter, config ConnConfig) *Conn {
	config = config.withDefaults()

	conn := &Conn{
		port:    port,
		config:  config,
		acks:    make(chan struct{}, 1),
		resets:  make(chan struct{}, 1),
		inbound: make(chan []byte, config.InboundQueue),
		done:    make(chan struct{}),
	}

	go conn.serve()

	return conn
}

// Inbound returns the payloads received from the module. The channel is closed when the
// connection terminates.
func (conn *Conn) Inbound() <-chan []byte {
	return conn.inbound
}

// Resets returns a channel that signals reset indications from the module, e.g. after it
// restarted. Signals are dropped if nobody is listening.
func (conn *Conn) Resets() <-chan struct{} {
	return conn.resets
}

// Reset resets the FT1.2 link: it sends a reset request and waits for its acknowledgement.
func (conn *Conn) Reset() error {
	conn.sendMu.Lock()
	defer conn.sendMu.Unlock()

	return conn.reset()
}

func (conn *Conn) reset() error {
	if err := conn.transmit(AppendFixed(nil, ControlResetReq)); err != nil {
		return err
	}

	// Data frames start with FCB = 1 after a reset.
	conn.fcb = true
	conn.synced = true

	conn.recvMu.Lock()
	conn.lastRecv = nil
	conn.recvMu.Unlock()

	return nil
}

// Send transmits the payload in a data frame and waits until the module acknowledges it. If the
// module stops responding, the link is reset before the next frame is sent.
func (conn *Conn) Send(payload []byte) error {
	if len(payload) > maxPayload {
		return ErrPayloadTooLong
	}

	conn.sendMu.Lock()
	defer conn.sendMu.Unlock()

	if !conn.synced {
		if err := conn.reset(); err != nil {
			return err
		}
	}

	control := byte(ControlHostData)
	if conn.fcb {
		control |= ControlFCB
	}

	if err := conn.transmit(AppendVariable(nil, control, payload)); err != nil {
		conn.synced = false
		return err
	}

	conn.fcb = !conn.fcb

	return nil
}

// transmit writes the frame and waits for the acknowledgement, resending the frame if needed.
func (conn *Conn) transmit(frame []byte) error {
	// Discard a stale acknowledgement.
	select {
	case <-conn.acks:
	default:
	}

	timer := time.NewTimer(conn.config.AckTimeout)
	defer timer.Stop()

	for attempt := 0; attempt <= conn.config.Retries; attempt++ {
		if err := conn.write(frame); err != nil {
			return err
		}

		if attempt > 0 {
			timer.Reset(conn.config.AckTimeout)
		}

		select {
		case <-conn.acks:
			return nil

		case <-timer.C:

		case <-conn.done:
			return ErrClosed
		}
	}

	return ErrNoAck
}

func (conn *Conn) write(data []byte) error {
	conn.writeMu.Lock()
	defer conn.writeMu.Unlock()

	select {
	case <-conn.done:
		return ErrClosed
	default:
	}

	_, err := conn.port.Write(data)

	return err
}

// Close terminates the connection. If the port implements io.Closer, it is closed too.
func (conn *Conn) Close() error {
	var err error

	conn.once.Do(func() {
		close(conn.done)

		if closer, ok := conn.port.(io.Closer); ok {
			err = closer.Close()
		}
	})

	return err
}

// Err returns the error that terminated the connection, if any.
func (conn *Conn) Err() error {
	select {
	case <-conn.done:
		return conn.readErr
	default:
		return nil
	}
}

func (conn *Conn) serve() {
	defer close(conn.inbound)

	var decoder Decoder
	buffer := make([]byte, 64)

	for {
		n, err := conn.port.Read(buffer)

		select {
		case <-conn.done:
			return
		default:
		}

		if err != nil {
			conn.readErr = err
			conn.Close()

			return
		}

		if n == 0 {
			time.Sleep(time.Millisecond)
			continue
		}

		decoder.Write(buffer[:n])

		for {
			frame, ok := decoder.Next()
			if !ok {
				break
			}

			conn.handle(frame)
		}
	}
}

func (conn *Conn) handle(frame Frame) {
	switch frame.Kind {
	case KindAck:
		select {
		case conn.acks <- struct{}{}:
		default:
		}

	case KindFixed:
		conn.write([]byte{Ack})

		if frame.Control == ControlResetInd {
			conn.recvMu.Lock()
			conn.lastRecv = nil
			conn.recvMu.Unlock()

			select {
			case conn.resets <- struct{}{}:
			default:
			}
		}

	case KindVariable:
		conn.write([]byte{Ack})

		// A frame with the same FCB and payload as the previous one is a retransmission, sent
		// because our acknowledgement got lost.
		fcb := frame.Control & ControlFCB

		conn.recvMu.Lock()
		duplicate := conn.lastRecv != nil && fcb == conn.lastFCB &&
			bytes.Equal(frame.Payload, conn.lastRecv)
		conn.lastRecv = frame.Payload
		conn.lastFCB = fcb
		conn.recvMu.Unlock()

		if duplicate {
			return
		}

		select {
		case conn.inbound <- frame.Payload:
		case <-conn.done:
		}
	}
}
