package tpuart

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/shabunin/tiny-knx-go/cemi"
	"github.com/shabunin/tiny-knx-go/link"
)

// bytePipe is one direction of an in-memory serial line. Like a UART it buffers written data, so
// that writers never wait for readers.
type bytePipe struct {
	data   chan []byte
	rest   []byte
	closed chan struct{}
	once   sync.Once
}

func newBytePipe() *bytePipe {
	return &bytePipe{data: make(chan []byte, 64), closed: make(chan struct{})}
}

func (p *bytePipe) Read(buffer []byte) (int, error) {
	if len(p.rest) == 0 {
		select {
		case p.rest = <-p.data:
		case <-p.closed:
			return 0, io.EOF
		}
	}

	n := copy(buffer, p.rest)
	p.rest = p.rest[n:]

	return n, nil
}

func (p *bytePipe) Write(data []byte) (int, error) {
	select {
	case <-p.closed:
		return 0, io.ErrClosedPipe
	case p.data <- append([]byte(nil), data...):
		return len(data), nil
	}
}

func (p *bytePipe) Close() error {
	p.once.Do(func() { close(p.closed) })
	return nil
}

// pipePort is one end of an in-memory serial line.
type pipePort struct {
	in, out *bytePipe
}

func (p *pipePort) Read(buffer []byte) (int, error) { return p.in.Read(buffer) }
func (p *pipePort) Write(data []byte) (int, error)  { return p.out.Write(data) }

func (p *pipePort) Close() error {
	p.in.Close()
	p.out.Close()

	return nil
}

func serialLine() (host, chip *pipePort) {
	toChip, toHost := newBytePipe(), newBytePipe()

	return &pipePort{in: toHost, out: toChip}, &pipePort{in: toChip, out: toHost}
}

var (
	unreachableAddr = cemi.NewIndividualAddr3(1, 1, 99)
	silentAddr      = cemi.NewIndividualAddr3(1, 1, 98)
)

// fakeChip emulates an NCN5120 (or a TP-UART2 without the dummy byte) on the host side.
type fakeChip struct {
	port  *pipePort
	dummy bool // U_SetAddress has a dummy byte

	mu        sync.Mutex
	mute      bool   // do not answer resets
	resets    int    // reset requests
	stateReqs int    // state requests
	address   []byte // the last U_SetAddress argument
	acks      []byte // U_AckInformation requests
	sent      [][]byte
	changed   chan struct{}
}

func newFakeChip(port *pipePort, dummy bool) *fakeChip {
	c := &fakeChip{port: port, dummy: dummy, changed: make(chan struct{}, 100)}
	go c.serve()

	return c
}

func (c *fakeChip) serve() {
	var (
		pending []byte // bytes of the current multi-byte service
		frame   []byte
		need    int // bytes still missing for the current service
	)

	buffer := make([]byte, 64)

	for {
		n, err := c.port.Read(buffer)
		if err != nil {
			return
		}

		for _, b := range buffer[:n] {
			if need > 0 {
				pending = append(pending, b)
				need--

				if need > 0 {
					continue
				}

				switch {
				case pending[0] == SetAddressReq:
					c.mu.Lock()
					c.address = append([]byte(nil), pending[1:3]...)
					c.mu.Unlock()

				case pending[0]&0xC0 == DataStartReq:
					frame = append(frame, b)

				case pending[0]&0xC0 == DataEndReq:
					frame = append(frame, b)
					c.transmit(frame)
					frame = nil
				}

				c.notify()

				continue
			}

			switch {
			case b == ResetReq:
				c.mu.Lock()
				c.resets++
				mute := c.mute
				c.mu.Unlock()

				if !mute {
					c.port.Write([]byte{ResetInd})
				}

			case b == StateReq:
				c.mu.Lock()
				c.stateReqs++
				c.mu.Unlock()
				c.port.Write([]byte{StateInd})

			case b == SetAddressReq:
				pending, need = []byte{b}, 2
				if c.dummy {
					need = 3
				}

			case b&0xF8 == AckInfoReq:
				c.mu.Lock()
				c.acks = append(c.acks, b)
				c.mu.Unlock()

			case b&0xF8 == DataOffsetReq:

			case b&0x80 == DataStartReq, b&0xC0 == DataEndReq:
				pending, need = []byte{b}, 1
			}

			c.notify()
		}
	}
}

// transmit puts a frame from the host on the "bus": the chip echoes it and confirms it.
func (c *fakeChip) transmit(frame []byte) {
	c.mu.Lock()
	c.sent = append(c.sent, append([]byte(nil), frame...))
	c.mu.Unlock()

	_, dest, group := frameAddresses(frame)

	if !group && cemi.IndividualAddr(dest) == silentAddr {
		return
	}

	c.port.Write(frame)

	if !group && cemi.IndividualAddr(dest) == unreachableAddr {
		c.port.Write([]byte{ConfirmInd})
	} else {
		c.port.Write([]byte{ConfirmInd | confirmSuccess})
	}
}

func (c *fakeChip) notify() {
	select {
	case c.changed <- struct{}{}:
	default:
	}
}

// waitFor polls cond until it holds or the test times out.
func (c *fakeChip) waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	timeout := time.After(2 * time.Second)

	for {
		c.mu.Lock()
		ok := cond()
		c.mu.Unlock()

		if ok {
			return
		}

		select {
		case <-c.changed:
		case <-time.After(10 * time.Millisecond):
		case <-timeout:
			t.Fatalf("Timed out waiting for %s", what)
		}
	}
}

func (c *fakeChip) takeAcks() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()

	acks := c.acks
	c.acks = nil

	return acks
}

var (
	linkAddr   = cemi.NewIndividualAddr3(1, 1, 50)
	testConfig = Config{
		Address:        linkAddr,
		ResetTimeout:   50 * time.Millisecond,
		ConfirmTimeout: 200 * time.Millisecond,
	}
)

func openTestLink(t *testing.T, config Config) (*Link, *fakeChip) {
	t.Helper()

	host, chipPort := serialLine()
	chip := newFakeChip(chipPort, config.Chip == ChipNCN5120)

	l, err := Open(host, config)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	t.Cleanup(func() {
		l.Close()
		chipPort.Close()
	})

	return l, chip
}

func TestOpen(t *testing.T) {
	l, chip := openTestLink(t, testConfig)

	chip.waitFor(t, "configuration", func() bool {
		return chip.resets == 1 && bytes.Equal(chip.address, []byte{0x11, 50}) && chip.stateReqs == 1
	})

	if l.Address() != linkAddr {
		t.Errorf("Address %v, expected %v", l.Address(), linkAddr)
	}
}

func TestOpenTPUART2(t *testing.T) {
	config := testConfig
	config.Chip = ChipTPUART2

	// The fake expects no dummy byte; with one, it would take 0xFF as the next service.
	_, chip := openTestLink(t, config)

	chip.waitFor(t, "configuration", func() bool {
		return bytes.Equal(chip.address, []byte{0x11, 50}) && chip.stateReqs == 1
	})
}

func TestOpenNoChip(t *testing.T) {
	host, chipPort := serialLine()
	defer chipPort.Close()

	chip := newFakeChip(chipPort, true)

	chip.mu.Lock()
	chip.mute = true
	chip.mu.Unlock()

	if _, err := Open(host, testConfig); !errors.Is(err, ErrNoResponse) {
		t.Errorf("Open: got %v, expected %v", err, ErrNoResponse)
	}

	chip.waitFor(t, "three reset requests", func() bool { return chip.resets == 3 })
}

func groupWrite(dest uint16, value byte) *cemi.LDataReq {
	return &cemi.LDataReq{LData: cemi.LData{
		Control1:    cemi.Control1Request(cemi.PrioLow),
		Control2:    cemi.Control2GroupAddr | cemi.Control2Hops(6),
		Destination: dest,
		Data:        &cemi.AppData{Command: cemi.GroupValueWrite, Data: []byte{value}},
	}}
}

func individualRequest(dest cemi.IndividualAddr) *cemi.LDataReq {
	return &cemi.LDataReq{LData: cemi.LData{
		Control1:    cemi.Control1Request(cemi.PrioLow),
		Control2:    cemi.Control2Hops(6),
		Destination: uint16(dest),
		Data:        &cemi.ControlData{Command: 0},
	}}
}

func expectNothing(t *testing.T, l *Link) {
	t.Helper()

	select {
	case ind := <-l.Inbound():
		t.Errorf("Unexpected frame %+v", ind.LData)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSend(t *testing.T) {
	l, chip := openTestLink(t, testConfig)

	if err := l.Send(groupWrite(0x0901, 1)); err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	// The source address is filled in.
	want := []byte{0xbc, 0x11, 50, 0x09, 0x01, 0xe1, 0x00, 0x81}
	want = append(want, Checksum(want))

	chip.mu.Lock()
	if len(chip.sent) != 1 || !bytes.Equal(chip.sent[0], want) {
		t.Errorf("Chip sent % x, expected % x", chip.sent, want)
	}
	chip.mu.Unlock()

	// The echo is neither acknowledged nor delivered.
	expectNothing(t, l)

	if acks := chip.takeAcks(); len(acks) != 0 {
		t.Errorf("Acknowledged our own frame: % x", acks)
	}

	if err := l.Send(individualRequest(unreachableAddr)); !errors.Is(err, link.ErrNotConfirmed) {
		t.Errorf("Send to unreachable device: got %v, expected %v", err, link.ErrNotConfirmed)
	}

	if err := l.Send(individualRequest(silentAddr)); !errors.Is(err, link.ErrConfirmTimeout) {
		t.Errorf("Send without confirmation: got %v, expected %v", err, link.ErrConfirmTimeout)
	}

	// Sending works again after a timeout.
	if err := l.Send(groupWrite(0x0901, 0)); err != nil {
		t.Errorf("Send after timeout: %v", err)
	}
}

// busFrame returns a frame from another device.
func busFrame(t *testing.T, dest uint16, group bool, value byte) []byte {
	t.Helper()

	ldata := cemi.LData{
		Control1:    cemi.Control1Request(cemi.PrioLow),
		Control2:    cemi.Control2Hops(6),
		Source:      cemi.NewIndividualAddr3(1, 1, 5),
		Destination: dest,
		Data:        &cemi.AppData{Command: cemi.GroupValueWrite, Data: []byte{value}},
	}

	if group {
		ldata.Control2 |= cemi.Control2GroupAddr
	}

	frame, err := AppendFrame(nil, &ldata)
	if err != nil {
		t.Fatal(err)
	}

	return frame
}

func receive(t *testing.T, l *Link) *cemi.LDataInd {
	t.Helper()

	select {
	case ind := <-l.Inbound():
		return ind
	case <-time.After(time.Second):
		t.Fatal("No frame received")
	}

	return nil
}

func TestReceive(t *testing.T) {
	config := testConfig
	config.AcceptGroup = func(ga cemi.GroupAddr) bool { return ga != 0x0902 }

	l, chip := openTestLink(t, config)
	chip.waitFor(t, "configuration", func() bool { return chip.stateReqs == 1 })

	addressed := []byte{AckInfoReq, AckInfoReq | AckInfoAddressed}

	cases := []struct {
		name    string
		frame   []byte
		deliver bool
	}{
		{"group", busFrame(t, 0x0901, true, 1), true},
		{"broadcast", busFrame(t, 0, true, 1), true},
		{"to us", busFrame(t, uint16(linkAddr), false, 1), true},
		{"to another device", busFrame(t, uint16(unreachableAddr), false, 1), false},
		{"rejected group", busFrame(t, 0x0902, true, 1), false},
	}

	for _, tc := range cases {
		chip.port.Write(tc.frame)

		if tc.deliver {
			ind := receive(t, l)
			if ind.Destination != func() uint16 { _, d, _ := frameAddresses(tc.frame); return d }() {
				t.Errorf("%s: delivered %+v", tc.name, ind.LData)
			}

			chip.waitFor(t, tc.name+" acknowledgement", func() bool { return len(chip.acks) == 2 })

			if acks := chip.takeAcks(); !bytes.Equal(acks, addressed) {
				t.Errorf("%s: acknowledgement % x, expected % x", tc.name, acks, addressed)
			}
		} else {
			expectNothing(t, l)

			if acks := chip.takeAcks(); !bytes.Equal(acks, []byte{AckInfoReq}) {
				t.Errorf("%s: acknowledgement % x, expected only % x", tc.name, acks, AckInfoReq)
			}
		}
	}
}

func TestReceiveAllFrames(t *testing.T) {
	config := testConfig
	config.AllFrames = true

	l, chip := openTestLink(t, config)
	chip.waitFor(t, "configuration", func() bool { return chip.stateReqs == 1 })

	chip.port.Write(busFrame(t, uint16(unreachableAddr), false, 1))

	if ind := receive(t, l); cemi.IndividualAddr(ind.Destination) != unreachableAddr {
		t.Errorf("Delivered %+v", ind.LData)
	}

	time.Sleep(20 * time.Millisecond)

	if acks := chip.takeAcks(); !bytes.Equal(acks, []byte{AckInfoReq}) {
		t.Errorf("Frame for another device acknowledged: % x", acks)
	}
}

func TestRepetitionDropped(t *testing.T) {
	l, chip := openTestLink(t, testConfig)

	frame := busFrame(t, 0x0901, true, 1)
	repeated := append([]byte(nil), frame...)
	repeated[0] &^= ctrlNotRepeat
	repeated[len(repeated)-1] = Checksum(repeated[:len(repeated)-1])

	chip.port.Write(frame)
	receive(t, l)

	chip.port.Write(repeated)
	expectNothing(t, l)

	// A repetition of a frame that we missed is delivered.
	other := busFrame(t, 0x0901, true, 0)
	other[0] &^= ctrlNotRepeat
	other[len(other)-1] = Checksum(other[:len(other)-1])

	chip.port.Write(other)

	if ind := receive(t, l); ind.Data.(*cemi.AppData).Data[0] != 0 {
		t.Errorf("Delivered %+v", ind.LData)
	}
}

func TestResync(t *testing.T) {
	l, chip := openTestLink(t, testConfig)
	frame := busFrame(t, 0x0901, true, 1)

	broken := append([]byte(nil), frame...)
	broken[len(broken)-1] ^= 0xff

	// The frame directly after a broken one is lost, as its bytes cannot be told apart from the
	// rest of the broken frame.
	chip.port.Write(append(broken, frame...))
	expectNothing(t, l)

	chip.port.Write(frame)
	receive(t, l)

	// The same after a byte that is no known service.
	chip.port.Write(append([]byte{0x0a}, frame...))
	expectNothing(t, l)

	chip.port.Write(frame)
	receive(t, l)
}

func TestSetAddress(t *testing.T) {
	l, chip := openTestLink(t, testConfig)
	newAddr := cemi.NewIndividualAddr3(1, 2, 3)

	if err := l.SetAddress(newAddr); err != nil {
		t.Fatal(err)
	}

	chip.waitFor(t, "new address", func() bool { return bytes.Equal(chip.address, []byte{0x12, 3}) })

	if l.Address() != newAddr {
		t.Errorf("Address %v, expected %v", l.Address(), newAddr)
	}

	chip.port.Write(busFrame(t, uint16(linkAddr), false, 1))
	expectNothing(t, l)

	chip.port.Write(busFrame(t, uint16(newAddr), false, 1))
	receive(t, l)

	if err := l.Send(groupWrite(0x0901, 1)); err != nil {
		t.Fatal(err)
	}

	chip.mu.Lock()
	src, _, _ := frameAddresses(chip.sent[0])
	chip.mu.Unlock()

	if src != newAddr {
		t.Errorf("Sent with source %v, expected %v", src, newAddr)
	}
}

func TestChipReset(t *testing.T) {
	l, chip := openTestLink(t, testConfig)
	chip.waitFor(t, "configuration", func() bool { return chip.stateReqs == 1 })

	chip.mu.Lock()
	chip.address = nil
	chip.mu.Unlock()

	// The chip restarts on its own and needs its address again.
	chip.port.Write([]byte{ResetInd})

	chip.waitFor(t, "configuration", func() bool {
		return bytes.Equal(chip.address, []byte{0x11, 50}) && chip.stateReqs == 2
	})

	chip.port.Write([]byte{StateReceiveError | StateInd})

	deadline := time.Now().Add(time.Second)
	for l.State() != StateReceiveError {
		if time.Now().After(deadline) {
			t.Fatalf("State %#x, expected %#x", l.State(), StateReceiveError)
		}

		time.Sleep(time.Millisecond)
	}
}

func TestClose(t *testing.T) {
	l, _ := openTestLink(t, testConfig)

	l.Close()

	if _, open := <-l.Inbound(); open {
		t.Error("Inbound still open after Close")
	}

	if err := l.Send(groupWrite(0x0901, 1)); !errors.Is(err, link.ErrClosed) {
		t.Errorf("Send after Close: got %v, expected %v", err, link.ErrClosed)
	}
}
