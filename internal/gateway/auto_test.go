package gateway

import (
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shabunin/tiny-knx-go/baos"
	"github.com/shabunin/tiny-knx-go/cemi"
	"github.com/shabunin/tiny-knx-go/link/mock"
)

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

func groupWrite(value byte) *cemi.LDataReq {
	return &cemi.LDataReq{LData: cemi.LData{
		Control1:    cemi.Control1Request(cemi.PrioLow),
		Control2:    cemi.Control2GroupAddr | cemi.Control2Hops(6),
		Destination: 0x0901,
		Data:        &cemi.AppData{Command: cemi.GroupValueWrite, Data: []byte{value}},
	}}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	for deadline := time.Now().Add(2 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func TestAutoModule(t *testing.T) {
	realLine := mock.NewBus()
	moduleAddr := cemi.NewIndividualAddr3(1, 4, 1)

	var links, servers atomic.Int32

	a := NewAutoModule(AutoConfig{
		OpenLink: func() (Uplink, error) {
			links.Add(1)
			return realLine.Attach(moduleAddr), nil
		},
		OpenBAOS: func() (*baos.Client, io.Closer, error) {
			servers.Add(1)
			server := &objectServer{inbound: make(chan []byte, 4)}

			c, err := baos.NewClient(server, baos.Config{Version: baos.Version2})
			if err != nil {
				return nil, nil, err
			}

			return c, closerFunc(func() error { close(server.inbound); return nil }), nil
		},
		Idle:  50 * time.Millisecond,
		Retry: 50 * time.Millisecond,
		Log:   t.Logf,
	})
	defer a.Close()

	waitFor(t, "ObjectServer mode", func() bool { return a.Hub.Client() != nil })

	device := realLine.Attach(cemi.NewIndividualAddr3(1, 4, 5))
	tunnel := a.Attach(cemi.NewIndividualAddr3(1, 4, 250))

	// The first frame waits for the switch.
	if err := tunnel.Send(groupWrite(1)); err != nil {
		t.Fatal(err)
	}

	select {
	case ind := <-device.Inbound():
		if ind.Destination != 0x0901 {
			t.Errorf("%+v", ind.LData)
		}

	case <-time.After(time.Second):
		t.Fatal("the frame from the tunnel did not reach the line")
	}

	if a.Hub.Client() != nil || a.Line() == nil {
		t.Error("the module is not in data link layer mode")
	}

	device.Send(groupWrite(0))

	select {
	case ind := <-tunnel.Inbound():
		if ind.Source != device.Address() {
			t.Errorf("%+v", ind.LData)
		}

	case <-time.After(time.Second):
		t.Fatal("the frame from the line did not reach the tunnel")
	}

	// ETS reconnects within the idle time: the module stays in data link layer mode.
	tunnel.Close()
	tunnel = a.Attach(cemi.NewIndividualAddr3(1, 4, 250))
	time.Sleep(100 * time.Millisecond)

	if a.Line() == nil || links.Load() != 1 {
		t.Errorf("the module switched for a reconnection, %d links", links.Load())
	}

	tunnel.Close()
	waitFor(t, "ObjectServer mode again", func() bool { return a.Hub.Client() != nil })

	if a.Line() != nil || servers.Load() != 2 {
		t.Errorf("line %v, %d ObjectServers", a.Line(), servers.Load())
	}
}
