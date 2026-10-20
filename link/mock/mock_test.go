package mock

import (
	"errors"
	"testing"

	"github.com/shabunin/tiny-knx-go/cemi"
	"github.com/shabunin/tiny-knx-go/link"
)

func frame(dest uint16, group bool) *cemi.LDataReq {
	control2 := cemi.Control2Hops(6)
	if group {
		control2 |= cemi.Control2GroupAddr
	}

	return &cemi.LDataReq{LData: cemi.LData{
		Control1:    cemi.Control1StdFrame,
		Control2:    control2,
		Destination: dest,
		Data:        &cemi.AppData{Command: cemi.GroupValueWrite, Data: []byte{1}},
	}}
}

func TestBus(t *testing.T) {
	bus := NewBus()
	a := bus.Attach(cemi.NewIndividualAddr3(1, 1, 1))
	b := bus.Attach(cemi.NewIndividualAddr3(1, 1, 2))
	c := bus.Attach(cemi.NewIndividualAddr3(1, 1, 3))

	if err := a.Send(frame(uint16(cemi.NewGroupAddr3(1, 1, 1)), true)); err != nil {
		t.Fatalf("Group send failed: %v", err)
	}

	for _, l := range []*Link{b, c} {
		ind := <-l.Inbound()
		if ind.Source != a.Address() {
			t.Errorf("Source %v, expected %v", ind.Source, a.Address())
		}
	}

	if len(a.Inbound()) != 0 {
		t.Error("Sender received its own frame")
	}

	if err := a.Send(frame(uint16(b.Address()), false)); err != nil {
		t.Errorf("Send to attached device failed: %v", err)
	}

	if err := a.Send(frame(uint16(cemi.NewIndividualAddr3(1, 1, 99)), false)); !errors.Is(err, link.ErrNotConfirmed) {
		t.Errorf("Send to missing device: got %v, expected %v", err, link.ErrNotConfirmed)
	}

	// Receivers must not share frames.
	x, y := <-b.Inbound(), <-c.Inbound()
	if x == y || x.Data == y.Data {
		t.Error("Receivers share the same frame")
	}

	c.Close()

	if err := c.Send(frame(1, true)); !errors.Is(err, link.ErrClosed) {
		t.Errorf("Send on closed link: got %v, expected %v", err, link.ErrClosed)
	}

	if err := a.Send(frame(uint16(c.Address()), false)); !errors.Is(err, link.ErrNotConfirmed) {
		t.Errorf("Send to closed device: got %v, expected %v", err, link.ErrNotConfirmed)
	}
}

func TestDrop(t *testing.T) {
	bus := NewBus()
	a := bus.Attach(cemi.NewIndividualAddr3(1, 1, 1))
	b := bus.Attach(cemi.NewIndividualAddr3(1, 1, 2))

	bus.Drop = func(cemi.IndividualAddr, *cemi.LData) bool { return true }

	if err := a.Send(frame(1, true)); err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	if len(b.Inbound()) != 0 {
		t.Error("Dropped frame was delivered")
	}
}
