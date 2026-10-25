package gateway

import (
	"io"
	"sync"
	"time"

	"github.com/shabunin/tiny-knx-go/baos"
	"github.com/shabunin/tiny-knx-go/cemi"
	"github.com/shabunin/tiny-knx-go/link"
	"github.com/shabunin/tiny-knx-go/link/mock"
)

// AutoConfig configures an AutoModule.
type AutoConfig struct {
	// OpenLink opens the module in data link layer mode. Closing the link must leave the module
	// where OpenBAOS can switch it to ObjectServer mode.
	OpenLink func() (Uplink, error)

	// OpenBAOS opens the ObjectServer of the module, which it switches to ObjectServer mode, and
	// returns what closes it.
	OpenBAOS func() (*baos.Client, io.Closer, error)

	// Idle is how long the module stays in data link layer mode after the last tunnel closed, so
	// that ETS can reconnect without switching back and forth.
	Idle time.Duration

	// Retry is how long to wait after a switch failed or the module went away.
	Retry time.Duration

	// Opened, if set, is called after every switch to ObjectServer mode.
	Opened func(c *baos.Client)

	// Log, if set, is called for switches and failures.
	Log func(format string, args ...any)
}

// switchTimeout is how long a frame from a tunnel waits for the module to switch to data link
// layer mode.
const switchTimeout = 5 * time.Second

// An AutoModule keeps a serial BAOS module in ObjectServer mode while no tunnel needs it, and in
// data link layer mode while one does, like an IP BAOS, which is an ObjectServer and a KNXnet/IP
// interface at once. A module is in one mode at a time: while ETS has a tunnel, the ObjectServer
// is gone and its clients are turned away.
type AutoModule struct {
	// Hub has the ObjectServer while the module is in ObjectServer mode.
	Hub *BAOSHub

	// Bus is the line that the tunnels reach, which reaches the module in data link layer mode.
	Bus *mock.Bus

	config AutoConfig
	up     *mock.Link
	wake   chan struct{}

	mu      sync.Mutex
	tunnels int
	line    Uplink
	client  *baos.Client
	closer  io.Closer
	ready   chan struct{} // closed when a switch to data link layer mode ends
	closed  bool
}

// NewAutoModule starts switching the module, which it puts in ObjectServer mode first.
func NewAutoModule(config AutoConfig) *AutoModule {
	if config.Log == nil {
		config.Log = func(string, ...any) {}
	}

	a := &AutoModule{
		Hub:    NewBAOSHub(),
		Bus:    mock.NewBus(),
		config: config,
		wake:   make(chan struct{}, 1),
	}

	a.up = a.Bus.AttachUplink()

	go a.toLine()
	go a.run()

	return a
}

// Attach connects a tunnel to the line. While a tunnel is open, the module is in data link layer
// mode.
func (a *AutoModule) Attach(addr cemi.IndividualAddr) link.Link {
	a.mu.Lock()
	a.tunnels++
	if a.line == nil && a.ready == nil {
		a.ready = make(chan struct{})
	}
	a.mu.Unlock()

	a.poke()

	return &autoTunnel{Link: a.Bus.Attach(addr), a: a}
}

// Line returns the module in data link layer mode, or nil.
func (a *AutoModule) Line() Uplink {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.line
}

// Close closes the module in the mode it is in.
func (a *AutoModule) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.closed = true

	if a.line != nil {
		a.line.Close()
	}

	if a.closer != nil {
		a.closer.Close()
	}

	a.line, a.client, a.closer = nil, nil, nil
	a.endSwitch()

	return nil
}

type autoTunnel struct {
	link.Link
	a    *AutoModule
	once sync.Once
}

func (t *autoTunnel) Close() error {
	t.once.Do(func() {
		a := t.a

		a.mu.Lock()
		a.tunnels--
		if a.tunnels == 0 {
			a.endSwitch()
		}
		a.mu.Unlock()

		a.poke()
	})

	return t.Link.Close()
}

func (a *AutoModule) poke() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// endSwitch releases the frames that wait for the module to switch to data link layer mode. It is
// called with mu held.
func (a *AutoModule) endSwitch() {
	if a.ready != nil {
		close(a.ready)
		a.ready = nil
	}
}

// toLine passes the frames from the tunnels to the module, which they wait for while it switches
// to data link layer mode. In ObjectServer mode they are dropped.
func (a *AutoModule) toLine() {
	for ind := range a.up.Inbound() {
		a.mu.Lock()
		ready := a.ready
		a.mu.Unlock()

		if ready != nil {
			select {
			case <-ready:
			case <-time.After(switchTimeout):
			}
		}

		if line := a.Line(); line != nil {
			line.Send(&cemi.LDataReq{LData: ind.LData})
		}
	}
}

// run switches the module between its modes.
func (a *AutoModule) run() {
	var (
		idle    <-chan time.Time // runs while the module waits in data link layer mode without tunnels
		expired bool             // idle ran out
		retry   <-chan time.Time // runs after a failure
		gone    <-chan struct{}  // closed when the ObjectServer goes
	)

	for {
		a.mu.Lock()
		want, line, client, closed := a.tunnels > 0, a.line, a.client, a.closed
		a.mu.Unlock()

		if closed {
			return
		}

		switch {
		case retry != nil:

		case want && line == nil:
			idle, expired = nil, false

			if client != nil {
				gone = nil
				a.closeBAOS()
			}

			if err := a.openLink(); err != nil {
				a.config.Log("Switching the module to data link layer mode: %v", err)
				retry = time.After(a.config.Retry)
			}

		case want:
			idle, expired = nil, false

		case line != nil && !expired:
			if idle == nil {
				idle = time.After(a.config.Idle)
			}

		case client == nil:
			if line != nil {
				a.closeLink(line)
			}

			expired = false

			g, err := a.openBAOS()
			if err != nil {
				a.config.Log("Switching the module to ObjectServer mode: %v", err)
				retry = time.After(a.config.Retry)
			}

			gone = g
		}

		select {
		case <-a.wake:
		case <-idle:
			idle, expired = nil, true
		case <-retry:
			retry = nil
		case <-gone:
			gone = nil

			a.mu.Lock()
			closed := a.closed
			a.mu.Unlock()

			if closed {
				return
			}

			a.config.Log("The ObjectServer went away")
			a.closeBAOS()
			retry = time.After(a.config.Retry)
		}
	}
}

func (a *AutoModule) openLink() error {
	line, err := a.config.OpenLink()

	a.mu.Lock()
	defer a.mu.Unlock()

	a.endSwitch()

	if err != nil {
		return err
	}

	if a.closed {
		line.Close()
		return nil
	}

	a.line = line
	a.config.Log("The module is in data link layer mode at %v, for the tunnels", line.Address())

	go func() {
		for ind := range line.Inbound() {
			a.up.Send(&cemi.LDataReq{LData: ind.LData})
		}

		a.mu.Lock()
		if a.line == line {
			a.line = nil
			a.config.Log("The connection to the module in data link layer mode closed")
		}
		a.mu.Unlock()

		a.poke()
	}()

	return nil
}

func (a *AutoModule) closeLink(line Uplink) {
	a.mu.Lock()
	a.line = nil
	a.mu.Unlock()

	line.Close()
}

func (a *AutoModule) openBAOS() (<-chan struct{}, error) {
	c, closer, err := a.config.OpenBAOS()
	if err != nil {
		return nil, err
	}

	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		closer.Close()

		return nil, nil
	}

	a.client, a.closer = c, closer
	a.mu.Unlock()

	gone := a.Hub.Use(c)
	a.config.Log("The module is in ObjectServer mode")

	if a.config.Opened != nil {
		a.config.Opened(c)
	}

	return gone, nil
}

func (a *AutoModule) closeBAOS() {
	a.mu.Lock()
	client, closer := a.client, a.closer
	a.client, a.closer = nil, nil
	a.mu.Unlock()

	if client != nil {
		a.Hub.release(client)
	}

	if closer != nil {
		closer.Close()
	}
}
