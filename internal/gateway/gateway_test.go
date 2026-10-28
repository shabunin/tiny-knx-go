package gateway

import (
	"bufio"
	"bytes"
	"net"
	"testing"
	"time"

	"github.com/shabunin/tiny-knx-go/baos"
	"github.com/shabunin/tiny-knx-go/cemi"
	"github.com/shabunin/tiny-knx-go/knxnet"
	"github.com/shabunin/tiny-knx-go/link/knxip"
	"github.com/shabunin/tiny-knx-go/link/mock"
)

func TestParseSpec(t *testing.T) {
	s, err := ParseSpec("ft12:/dev/ttyAMA0?baud=115200&device")
	if err != nil {
		t.Fatal(err)
	}

	if s.Scheme != "ft12" || s.Target != "/dev/ttyAMA0" {
		t.Errorf("%+v", s)
	}

	if baud, err := s.Int("baud", 19200); err != nil || baud != 115200 {
		t.Errorf("baud %d, %v", baud, err)
	}

	if !s.Has("device") || s.Has("monitor") {
		t.Error("Has")
	}

	if err := s.Unused(); err != nil {
		t.Error(err)
	}

	s, _ = ParseSpec("baos:tcp://192.168.2.29:12004")
	if s.Scheme != "baos" || s.Target != "tcp://192.168.2.29:12004" {
		t.Errorf("%+v", s)
	}

	s, _ = ParseSpec("monitor")
	if s.Scheme != "monitor" || s.Target != "" {
		t.Errorf("%+v", s)
	}

	s, _ = ParseSpec("knxip:wlan0?tunnels=1.1.250,1.1.251&adress=1.1.0")
	if list, err := s.Addresses("tunnels"); err != nil || len(list) != 2 || list[1] != cemi.NewIndividualAddr3(1, 1, 251) {
		t.Errorf("tunnels %v, %v", list, err)
	}

	if err := s.Unused(); err == nil {
		t.Error("misspelled option accepted")
	}

	if _, err := s.Int("tunnels", 0); err == nil {
		t.Error("bad number accepted")
	}

	if _, err := ParseSpec(":x"); err == nil {
		t.Error("spec without medium accepted")
	}
}

func TestBAOSFrame(t *testing.T) {
	// Figure 11 of the protocol description: GetServerItem.Req over TCP.
	frame := appendBAOSFrame(nil, []byte{0xf0, 0x01, 0x00, 0x01, 0x00, 0x01})
	want := []byte{0x06, 0x20, 0xf0, 0x80, 0x00, 0x10, 0x04, 0x00, 0x00, 0x00, 0xf0, 0x01, 0x00, 0x01, 0x00, 0x01}

	if !bytes.Equal(frame, want) {
		t.Fatalf("% x", frame)
	}

	message, err := readBAOSFrame(bufio.NewReader(bytes.NewReader(frame)))
	if err != nil || !bytes.Equal(message, want[10:]) {
		t.Errorf("% x, %v", message, err)
	}

	frame[3] = 0x81
	if _, err := readBAOSFrame(bufio.NewReader(bytes.NewReader(frame))); err != errBAOSFrame {
		t.Errorf("wrong service: %v", err)
	}
}

// objectServer answers GetServerItem in version 2 with the firmware version for every item.
type objectServer struct {
	inbound chan []byte
}

func (s *objectServer) Inbound() <-chan []byte { return s.inbound }

func (s *objectServer) Send(req []byte) error {
	if len(req) != 6 || req[1] != baos.GetServerItem {
		return nil
	}

	res := append([]byte{0xf0, 0x81}, req[2:6]...)
	for id := 0; id < int(req[5]); id++ {
		res = append(res, 0, req[3]+byte(id), 1, 0x12)
	}

	s.inbound <- res

	return nil
}

// serveTCP serves the ObjectServer of the hub over TCP and returns the spec of a client.
func serveTCP(t *testing.T, hub *BAOSHub) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { l.Close() })

	go ServeBAOS(l, hub)

	return "baos:tcp://" + l.Addr().String()
}

// serveUDP serves the ObjectServer of the hub over KNXnet/IP and returns the spec of a client.
func serveUDP(t *testing.T, hub *BAOSHub) string {
	control, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}

	s := knxip.NewTunnelServer(control, nil, knxip.TunnelServerConfig{
		Server:       knxnet.ServerConfig{ObjectServer: 0x20},
		ObjectServer: hub.ServeKNXIP,
	})

	t.Cleanup(func() { s.Close() })

	return "baos:udp://" + control.LocalAddr().String()
}

func TestServeBAOS(t *testing.T) {
	t.Run("tcp", func(t *testing.T) { testServeBAOS(t, serveTCP) })
	t.Run("udp", func(t *testing.T) { testServeBAOS(t, serveUDP) })
}

func testServeBAOS(t *testing.T, serve func(*testing.T, *BAOSHub) string) {
	server := &objectServer{inbound: make(chan []byte, 4)}

	c, err := baos.NewClient(server, baos.Config{Version: baos.Version2})
	if err != nil {
		t.Fatal(err)
	}

	hub := NewBAOSHub()
	hub.Use(c)

	spec, _ := ParseSpec(serve(t, hub))

	remote, err := OpenBAOS(spec)
	if err != nil {
		t.Fatal(err)
	}

	if remote.Version() != baos.Version2 {
		t.Errorf("version %d", remote.Version())
	}

	items, err := remote.ServerItems(baos.ItemFirmwareVersion, 2)
	if err != nil || len(items) != 2 || items[1].ID != 4 || !bytes.Equal(items[1].Data, []byte{0x12}) {
		t.Fatalf("%+v, %v", items, err)
	}

	server.inbound <- []byte{0xf0, 0xc1, 0x00, 0x02, 0x00, 0x01, 0x00, 0x02, 0x18, 0x01, 0x01}

	select {
	case ind := <-remote.Indications():
		if len(ind.Values) != 1 || ind.Values[0].ID != 2 || ind.Values[0].Data[0] != 1 {
			t.Errorf("indication %+v", ind)
		}

	case <-time.After(time.Second):
		t.Fatal("no indication")
	}
}

func TestBridge(t *testing.T) {
	virtual, lineA, lineB := mock.NewBus(), mock.NewBus(), mock.NewBus()

	Bridge(virtual, lineA.Attach(cemi.NewIndividualAddr3(1, 1, 250)), "line A")
	Bridge(virtual, lineB.Attach(cemi.NewIndividualAddr3(1, 2, 250)), "line B")

	monitor := virtual.Attach(0)
	sender := lineA.Attach(cemi.NewIndividualAddr3(1, 1, 5))
	receiver := lineB.Attach(cemi.NewIndividualAddr3(1, 2, 5))

	err := sender.Send(&cemi.LDataReq{LData: cemi.LData{
		Control1:    cemi.Control1Request(cemi.PrioLow),
		Control2:    cemi.Control2GroupAddr | cemi.Control2Hops(6),
		Destination: 0x0901,
		Data:        &cemi.AppData{Command: cemi.GroupValueWrite, Data: []byte{1}},
	}})
	if err != nil {
		t.Fatal(err)
	}

	for name, l := range map[string]*mock.Link{"virtual line": monitor, "line B": receiver} {
		select {
		case ind := <-l.Inbound():
			if ind.Source != sender.Address() || ind.Destination != 0x0901 {
				t.Errorf("%s: %+v", name, ind.LData)
			}

		case <-time.After(time.Second):
			t.Errorf("%s: nothing received", name)
		}
	}
}
