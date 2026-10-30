package ft12

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
	return &bytePipe{data: make(chan []byte, 32), closed: make(chan struct{})}
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

func serialLine() (host, module *pipePort) {
	toModule, toHost := newBytePipe(), newBytePipe()

	return &pipePort{in: toHost, out: toModule}, &pipePort{in: toModule, out: toHost}
}

var (
	moduleAddr      = cemi.NewIndividualAddr3(1, 1, 240)
	unreachableAddr = cemi.NewIndividualAddr3(1, 1, 99)
)

// fakeModule emulates a BAOS module in data link layer mode.
type fakeModule struct {
	port *pipePort

	mu       sync.Mutex
	received []Frame // frames from the host, including retransmissions
	acks     int     // acknowledgements from the host
	drop     int     // number of upcoming data frames to ignore
	fcb      bool
	commMode byte
	address  cemi.IndividualAddr
	writes   int // property writes other than the communication mode
	changed  chan struct{}

	emi2    bool   // speaks EMI2 instead of cEMI
	emiMode []byte // last PEI_Switch.req
	noPCSet bool   // ignores PC_SetValue, like the BAOS Module 820
}

func newFakeModule(port *pipePort) *fakeModule {
	m := &fakeModule{port: port, commMode: 0xF0, address: moduleAddr, fcb: true, changed: make(chan struct{}, 100)}
	go m.serve()

	return m
}

func (m *fakeModule) serve() {
	var decoder Decoder
	buffer := make([]byte, 64)

	for {
		n, err := m.port.Read(buffer)
		if err != nil {
			return
		}

		decoder.Write(buffer[:n])

		for {
			frame, ok := decoder.Next()
			if !ok {
				break
			}

			m.handle(frame)
		}
	}
}

func (m *fakeModule) handle(frame Frame) {
	m.mu.Lock()

	if frame.Kind == KindAck {
		m.acks++
		m.mu.Unlock()
		m.notify()

		return
	}

	m.received = append(m.received, frame)

	if frame.Kind == KindVariable && m.drop > 0 {
		m.drop--
		m.mu.Unlock()

		return
	}

	m.mu.Unlock()
	m.port.Write([]byte{Ack})

	if frame.Kind == KindVariable {
		m.respond(frame.Payload)
	}

	m.notify()
}

func (m *fakeModule) notify() {
	select {
	case m.changed <- struct{}{}:
	default:
	}
}

func (m *fakeModule) respond(payload []byte) {
	if m.emi2 {
		m.respondEMI(payload)
		return
	}

	var msg cemi.Message
	if _, err := cemi.Unpack(payload, &msg); err != nil {
		return
	}

	switch req := msg.(type) {
	case *cemi.MPropWriteReq:
		con := &cemi.MPropWriteCon{MProp: req.MProp}
		con.Data = nil

		m.mu.Lock()
		switch {
		case req.ObjectType == cemi.ObjectTypeCEMIServer && req.PropertyID == cemi.PIDCommMode:
			m.commMode = req.Data[0]
		case req.ObjectType == cemi.ObjectTypeDevice && req.PropertyID == cemi.PIDSubnetAddr:
			m.address = cemi.IndividualAddr(req.Data[0])<<8 | m.address&0xff
			m.writes++
		case req.ObjectType == cemi.ObjectTypeDevice && req.PropertyID == cemi.PIDDeviceAddr:
			m.address = m.address&0xff00 | cemi.IndividualAddr(req.Data[0])
			m.writes++
		default:
			con.Count = 0
			con.Data = []byte{0x07}
		}
		m.mu.Unlock()

		m.send(con)

	case *cemi.MPropReadReq:
		con := &cemi.MPropReadCon{MProp: req.MProp}

		m.mu.Lock()
		switch {
		case req.ObjectType == cemi.ObjectTypeCEMIServer && req.PropertyID == cemi.PIDCommMode:
			con.Data = []byte{m.commMode}
		case req.ObjectType == cemi.ObjectTypeDevice && req.PropertyID == cemi.PIDDeviceDescriptor:
			con.Data = []byte{0x07, 0xb0}
		case req.ObjectType == cemi.ObjectTypeDevice && req.PropertyID == cemi.PIDSubnetAddr:
			con.Data = []byte{byte(m.address >> 8)}
		case req.ObjectType == cemi.ObjectTypeDevice && req.PropertyID == cemi.PIDDeviceAddr:
			con.Data = []byte{byte(m.address)}
		default:
			con.Count = 0
			con.Data = []byte{0x07}
		}
		m.mu.Unlock()

		m.send(con)

	case *cemi.LDataReq:
		con := &cemi.LDataCon{LData: req.LData}

		m.mu.Lock()
		con.Source = m.address
		m.mu.Unlock()

		if cemi.IndividualAddr(req.Destination) == unreachableAddr {
			con.Control1 |= cemi.Control1HasError
		}

		m.send(con)
	}
}

// send transmits a message to the host in a data frame.
func (m *fakeModule) send(msg cemi.Message) {
	payload := make([]byte, cemi.Size(msg))
	cemi.Pack(payload, msg)
	m.sendPayload(payload)
}

func (m *fakeModule) sendPayload(payload []byte) {
	m.port.Write(AppendVariable(nil, m.nextControl(), payload))
}

func (m *fakeModule) nextControl() byte {
	m.mu.Lock()
	defer m.mu.Unlock()

	control := byte(ControlModuleData)
	if m.fcb {
		control |= ControlFCB
	}

	m.fcb = !m.fcb

	return control
}

func (m *fakeModule) dataFrames() []Frame {
	m.mu.Lock()
	defer m.mu.Unlock()

	var frames []Frame
	for _, frame := range m.received {
		if frame.Kind == KindVariable {
			frames = append(frames, frame)
		}
	}

	return frames
}

// waitFor polls cond until it holds or the test times out.
func (m *fakeModule) waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	timeout := time.After(2 * time.Second)

	for {
		m.mu.Lock()
		ok := cond()
		m.mu.Unlock()

		if ok {
			return
		}

		select {
		case <-m.changed:
		case <-time.After(10 * time.Millisecond):
		case <-timeout:
			t.Fatalf("Timed out waiting for %s", what)
		}
	}
}

var testConfig = Config{
	Conn:           ConnConfig{AckTimeout: 50 * time.Millisecond, Retries: 3},
	ConfirmTimeout: 500 * time.Millisecond,
}

func openTestLink(t *testing.T) (*Link, *fakeModule) {
	t.Helper()

	host, modulePort := serialLine()
	module := newFakeModule(modulePort)

	l, err := Open(host, testConfig)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	t.Cleanup(func() {
		l.Close()
		modulePort.Close()
	})

	return l, module
}

func groupWrite(dest uint16, value byte) *cemi.LDataReq {
	return &cemi.LDataReq{LData: cemi.LData{
		Control1:    cemi.Control1StdFrame | cemi.Control1Prio(cemi.PrioLow),
		Control2:    cemi.Control2GroupAddr | cemi.Control2Hops(6),
		Destination: dest,
		Data:        &cemi.AppData{Command: cemi.GroupValueWrite, Data: []byte{value}},
	}}
}

func TestOpenSwitchesToLinkLayer(t *testing.T) {
	_, module := openTestLink(t)

	module.mu.Lock()
	defer module.mu.Unlock()

	if len(module.received) < 2 || module.received[0].Kind != KindFixed ||
		module.received[0].Control != ControlResetReq {
		t.Fatalf("Expected a reset request first, got %+v", module.received)
	}

	first := module.received[1]
	if first.Control != ControlHostData|ControlFCB {
		t.Errorf("First data frame has control %#x, expected %#x", first.Control, ControlHostData|ControlFCB)
	}

	if module.commMode != cemi.CommModeDataLinkLayer {
		t.Errorf("Comm mode %#x, expected data link layer", module.commMode)
	}
}

func TestSend(t *testing.T) {
	l, module := openTestLink(t)

	if l.Address() != 0 {
		t.Errorf("Address %v before the first confirmation", l.Address())
	}

	if err := l.Send(groupWrite(uint16(cemi.NewGroupAddr3(1, 2, 3)), 1)); err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	if l.Address() != moduleAddr {
		t.Errorf("Address %v, expected %v", l.Address(), moduleAddr)
	}

	err := l.Send(&cemi.LDataReq{LData: cemi.LData{
		Control1:    cemi.Control1StdFrame,
		Control2:    cemi.Control2Hops(6),
		Destination: uint16(unreachableAddr),
		Data:        &cemi.ControlData{Command: 0},
	}})
	if !errors.Is(err, link.ErrNotConfirmed) {
		t.Errorf("Send to unreachable device: got %v, expected %v", err, link.ErrNotConfirmed)
	}

	// Data frames must alternate the frame count bit.
	frames := module.dataFrames()
	for i := 1; i < len(frames); i++ {
		if frames[i].Control == frames[i-1].Control {
			t.Errorf("Frames %d and %d both have control %#x", i-1, i, frames[i].Control)
		}
	}

	// Every confirmation from the module must have been acknowledged.
	module.waitFor(t, "acknowledgements", func() bool { return module.acks == 4 })
}

func TestSendRetransmits(t *testing.T) {
	l, module := openTestLink(t)

	module.mu.Lock()
	module.drop = 2
	module.mu.Unlock()

	if err := l.Send(groupWrite(0x0a03, 1)); err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	// Comm mode read and write plus three copies of the group write.
	frames := module.dataFrames()
	if len(frames) != 5 {
		t.Fatalf("Module received %d data frames, expected 5", len(frames))
	}

	for _, frame := range frames[3:] {
		if frame.Control != frames[2].Control || !bytes.Equal(frame.Payload, frames[2].Payload) {
			t.Errorf("Retransmission %+v differs from original %+v", frame, frames[2])
		}
	}
}

func TestSendNoAck(t *testing.T) {
	l, module := openTestLink(t)

	module.mu.Lock()
	module.drop = 100
	module.mu.Unlock()

	if err := l.Send(groupWrite(0x0a03, 1)); !errors.Is(err, ErrNoAck) {
		t.Fatalf("Send: got %v, expected %v", err, ErrNoAck)
	}

	module.mu.Lock()
	module.drop = 0
	module.mu.Unlock()

	// The link must resynchronise with a reset before the next frame.
	before := len(module.received)

	if err := l.Send(groupWrite(0x0a03, 0)); err != nil {
		t.Fatalf("Send after failure: %v", err)
	}

	module.mu.Lock()
	next := module.received[before]
	module.mu.Unlock()

	if next.Kind != KindFixed || next.Control != ControlResetReq {
		t.Errorf("Expected a reset request before the next frame, got %+v", next)
	}
}

func TestInbound(t *testing.T) {
	l, module := openTestLink(t)

	ind := &cemi.LDataInd{LData: groupWrite(0x0a03, 1).LData}
	ind.Source = cemi.NewIndividualAddr3(1, 1, 1)

	payload := make([]byte, cemi.Size(ind))
	cemi.Pack(payload, ind)

	// The same frame twice with the same FCB is a retransmission and must be delivered once.
	control := module.nextControl()
	module.port.Write(AppendVariable(nil, control, payload))
	module.port.Write(AppendVariable(nil, control, payload))
	module.sendPayload(payload)

	for i := 0; i < 2; i++ {
		select {
		case msg := <-l.Inbound():
			if msg.Source != ind.Source || msg.Destination != ind.Destination {
				t.Errorf("Received %+v, expected %+v", msg, ind)
			}

		case <-time.After(time.Second):
			t.Fatalf("Frame %d not received", i)
		}
	}

	select {
	case msg := <-l.Inbound():
		t.Errorf("Unexpected extra frame %+v", msg)
	case <-time.After(100 * time.Millisecond):
	}

	module.waitFor(t, "acknowledgements", func() bool { return module.acks == 5 })
}

func TestReadProperty(t *testing.T) {
	l, _ := openTestLink(t)

	data, err := l.ReadProperty(cemi.ObjectTypeDevice, 1, cemi.PIDDeviceDescriptor, 1, 1)
	if err != nil {
		t.Fatalf("ReadProperty failed: %v", err)
	}

	if !bytes.Equal(data, []byte{0x07, 0xb0}) {
		t.Errorf("Device descriptor % x, expected 07 b0", data)
	}

	if _, err := l.ReadProperty(cemi.ObjectTypeDevice, 1, 200, 1, 1); !errors.Is(err, ErrPropertyFailed) {
		t.Errorf("Reading unknown property: got %v, expected %v", err, ErrPropertyFailed)
	}
}

func TestSetAddress(t *testing.T) {
	l, module := openTestLink(t)
	newAddr := cemi.NewIndividualAddr3(1, 2, 3)

	if err := l.SetAddress(newAddr); err != nil {
		t.Fatalf("SetAddress failed: %v", err)
	}

	if l.Address() != newAddr {
		t.Errorf("Address %v, expected %v", l.Address(), newAddr)
	}

	module.mu.Lock()
	if module.address != newAddr || module.writes != 2 {
		t.Errorf("Module has address %v after %d writes", module.address, module.writes)
	}
	module.mu.Unlock()

	// The same address again is not written.
	if err := l.SetAddress(newAddr); err != nil {
		t.Fatalf("SetAddress failed: %v", err)
	}

	module.mu.Lock()
	if module.writes != 2 {
		t.Errorf("Unchanged address written again (%d writes)", module.writes)
	}
	module.mu.Unlock()

	if err := l.Send(groupWrite(0x0901, 1)); err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	if l.Address() != newAddr {
		t.Errorf("Address %v after sending, expected %v", l.Address(), newAddr)
	}
}

func TestModuleResetRestoresLinkLayer(t *testing.T) {
	_, module := openTestLink(t)

	module.mu.Lock()
	module.commMode = 0xF0
	module.fcb = true
	module.mu.Unlock()

	module.port.Write(AppendFixed(nil, ControlResetInd))

	module.waitFor(t, "comm mode restored", func() bool {
		return module.commMode == cemi.CommModeDataLinkLayer
	})
}

func TestCloseRestoresCommMode(t *testing.T) {
	l, module := openTestLink(t)

	l.Close()

	// The module was in ObjectServer (BAOS) mode before Open.
	module.waitFor(t, "ObjectServer mode", func() bool { return module.commMode == cemi.CommModeObjectServer })
}

func TestClose(t *testing.T) {
	l, _ := openTestLink(t)

	l.Close()

	select {
	case _, open := <-l.Inbound():
		if open {
			t.Error("Inbound delivered a frame after Close")
		}
	case <-time.After(time.Second):
		t.Fatal("Inbound not closed after Close")
	}

	if err := l.Send(groupWrite(0x0a03, 1)); !errors.Is(err, link.ErrClosed) {
		t.Errorf("Send after Close: got %v, expected %v", err, link.ErrClosed)
	}
}

// After a module restarts, its frame count starts again: the next frame follows a reset and has
// FCB = 1, else the module may take it for a repetition and drop it.
func TestConnResetsAfterModuleRestart(t *testing.T) {
	host, port := serialLine()
	module := newFakeModule(port)

	conn := NewConn(host, ConnConfig{})
	defer conn.Close()

	if err := conn.Reset(); err != nil {
		t.Fatal(err)
	}

	if err := conn.Send([]byte{0xf0, 0x01, 0x01, 0x01}); err != nil { // FCB = 1, the next has 0
		t.Fatal(err)
	}

	module.port.Write(AppendFixed(nil, ControlResetInd))

	select {
	case <-conn.Resets():
	case <-time.After(time.Second):
		t.Fatal("no reset indication")
	}

	if err := conn.Send([]byte{0xf0, 0x01, 0x01, 0x01}); err != nil {
		t.Fatal(err)
	}

	module.mu.Lock()
	defer module.mu.Unlock()

	n := len(module.received)
	if n < 2 || module.received[n-2].Kind != KindFixed || module.received[n-2].Control != ControlResetReq {
		t.Fatalf("no reset before the frame: %+v", module.received)
	}

	if module.received[n-1].Control&ControlFCB == 0 {
		t.Errorf("frame after the restart has FCB = 0")
	}
}
