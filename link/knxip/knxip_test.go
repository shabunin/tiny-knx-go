package knxip

import (
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/shabunin/tiny-knx-go/cemi"
	"github.com/shabunin/tiny-knx-go/knxnet"
	"github.com/shabunin/tiny-knx-go/link"
)

var (
	tunnelAddr      = cemi.NewIndividualAddr3(1, 1, 250)
	unreachableAddr = cemi.NewIndividualAddr3(1, 1, 99)
)

// fakeServer is a KNXnet/IP tunnelling server on localhost. It confirms every L_Data.req except
// those to unreachableAddr.
type fakeServer struct {
	t    *testing.T
	conn *net.UDPConn

	mu          sync.Mutex
	client      *net.UDPAddr
	channel     uint8
	seq         uint8 // of the server's tunnelling requests
	received    []knxnet.Service
	ignoreAcks  bool // don't answer tunnelling requests
	noConfirm   bool // acknowledge, but send no L_Data.con
	rejectNext  bool // reject the next connect request
	connections int
}

func newFakeServer(t *testing.T) *fakeServer {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}

	s := &fakeServer{t: t, conn: conn}
	t.Cleanup(func() { conn.Close() })

	go s.serve()

	return s
}

func (s *fakeServer) address() string {
	return s.conn.LocalAddr().String()
}

func (s *fakeServer) send(service knxnet.Service) {
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()

	s.conn.WriteToUDP(knxnet.Encode(service), client)
}

// indicate sends a cEMI message to the client in a tunnelling request.
func (s *fakeServer) indicate(msg cemi.Message) {
	s.mu.Lock()
	request := &knxnet.TunnellingRequest{Channel: s.channel, Sequence: s.seq, Message: msg}
	s.seq++
	s.mu.Unlock()

	s.send(request)
}

func (s *fakeServer) serve() {
	buffer := make([]byte, 512)

	for {
		n, from, err := s.conn.ReadFromUDP(buffer)
		if err != nil {
			return
		}

		service, err := knxnet.Decode(buffer[:n])
		if err != nil {
			s.t.Errorf("Server received invalid packet %x: %v", buffer[:n], err)
			continue
		}

		s.mu.Lock()
		s.received = append(s.received, service)
		s.client = from
		s.mu.Unlock()

		s.handle(service)
	}
}

func (s *fakeServer) handle(service knxnet.Service) {
	s.mu.Lock()
	defer s.mu.Unlock()

	reply := func(r knxnet.Service) { s.conn.WriteToUDP(knxnet.Encode(r), s.client) }

	switch req := service.(type) {
	case *knxnet.ConnectRequest:
		if s.rejectNext {
			s.rejectNext = false
			reply(&knxnet.ConnectResponse{Status: knxnet.StatusNoMoreConnections})

			return
		}

		s.connections++
		s.channel++
		s.seq = 0
		reply(&knxnet.ConnectResponse{Channel: s.channel, Type: knxnet.TunnelConnection, Address: tunnelAddr})

	case *knxnet.ConnectionStateRequest:
		reply(&knxnet.ConnectionStateResponse{Channel: req.Channel})

	case *knxnet.DisconnectRequest:
		reply(&knxnet.DisconnectResponse{Channel: req.Channel})

	case *knxnet.TunnellingRequest:
		if s.ignoreAcks {
			return
		}

		reply(&knxnet.TunnellingAck{Channel: req.Channel, Sequence: req.Sequence})

		ldata, ok := req.Message.(*cemi.LDataReq)
		if !ok || s.noConfirm {
			return
		}

		con := &cemi.LDataCon{LData: ldata.LData}
		con.Source = tunnelAddr

		if !con.Control2.IsGroupAddr() && cemi.IndividualAddr(con.Destination) == unreachableAddr {
			con.Control1 |= cemi.Control1HasError
		}

		reply(&knxnet.TunnellingRequest{Channel: s.channel, Sequence: s.seq, Message: con})
		s.seq++
	}
}

// count returns how many packets of the type of example the server received.
func (s *fakeServer) count(example knxnet.Service) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	n := 0
	for _, r := range s.received {
		if r.ServiceID() == example.ServiceID() {
			n++
		}
	}

	return n
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("Timed out waiting for %s", what)
		}

		time.Sleep(time.Millisecond)
	}
}

var testConfig = TunnelConfig{
	Tunnel: knxnet.TunnelConfig{
		ConnectTimeout:    200 * time.Millisecond,
		AckTimeout:        50 * time.Millisecond,
		DisconnectTimeout: 100 * time.Millisecond,
	},
	ConfirmTimeout: 100 * time.Millisecond,
	ReconnectDelay: 50 * time.Millisecond,
}

func dial(t *testing.T, s *fakeServer) *Tunnel {
	t.Helper()

	tun, err := DialTunnel(s.address(), testConfig)
	if err != nil {
		t.Fatalf("DialTunnel: %v", err)
	}

	t.Cleanup(func() { tun.Close() })

	return tun
}

func groupWrite(value byte) *cemi.LDataReq {
	return &cemi.LDataReq{LData: cemi.LData{
		Control1:    cemi.Control1Request(cemi.PrioLow),
		Control2:    cemi.Control2GroupAddr | cemi.Control2Hops(6),
		Destination: 0x0901,
		Data:        &cemi.AppData{Command: cemi.GroupValueWrite, Data: []byte{value}},
	}}
}

func TestTunnelConnects(t *testing.T) {
	s := newFakeServer(t)
	tun := dial(t, s)

	if tun.Address() != tunnelAddr {
		t.Errorf("Address %v, expected %v", tun.Address(), tunnelAddr)
	}

	tun.Close()

	if s.count(&knxnet.DisconnectRequest{}) != 1 {
		t.Error("Close sent no disconnect request")
	}

	if _, open := <-tun.Inbound(); open {
		t.Error("Inbound still open after Close")
	}

	if err := tun.Send(groupWrite(1)); !errors.Is(err, link.ErrClosed) {
		t.Errorf("Send after Close: %v", err)
	}
}

func TestTunnelRejected(t *testing.T) {
	s := newFakeServer(t)
	s.mu.Lock()
	s.rejectNext = true
	s.mu.Unlock()

	if _, err := DialTunnel(s.address(), testConfig); !errors.Is(err, knxnet.StatusNoMoreConnections) {
		t.Errorf("DialTunnel: %v", err)
	}
}

func TestTunnelNoServer(t *testing.T) {
	s := newFakeServer(t)
	addr := s.address()
	s.conn.Close()

	if _, err := DialTunnel(addr, testConfig); !errors.Is(err, knxnet.ErrConnectTimeout) {
		t.Errorf("DialTunnel: %v", err)
	}
}

func TestTunnelSend(t *testing.T) {
	s := newFakeServer(t)
	tun := dial(t, s)

	for i := 0; i < 3; i++ {
		if err := tun.Send(groupWrite(byte(i))); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}

	unreachable := groupWrite(1)
	unreachable.Control2 = cemi.Control2Hops(6)
	unreachable.Destination = uint16(unreachableAddr)

	if err := tun.Send(unreachable); !errors.Is(err, link.ErrNotConfirmed) {
		t.Errorf("Send to unreachable device: %v", err)
	}

	s.mu.Lock()
	s.noConfirm = true
	s.mu.Unlock()

	if err := tun.Send(groupWrite(1)); !errors.Is(err, link.ErrConfirmTimeout) {
		t.Errorf("Send without confirmation: %v", err)
	}
}

func TestTunnelReceive(t *testing.T) {
	s := newFakeServer(t)
	tun := dial(t, s)

	s.indicate(&cemi.LDataInd{LData: groupWrite(1).LData})
	s.indicate(&cemi.LDataCon{LData: groupWrite(2).LData}) // not ours: ignored

	s.indicate(&cemi.LDataInd{LData: groupWrite(3).LData})

	for _, value := range []byte{1, 3} {
		select {
		case ind := <-tun.Inbound():
			if data := ind.Data.(*cemi.AppData).Data; data[0] != value {
				t.Errorf("Received value %d, expected %d", data[0], value)
			}
		case <-time.After(time.Second):
			t.Fatal("Nothing received")
		}
	}

	eventually(t, "acknowledgements", func() bool { return s.count(&knxnet.TunnellingAck{}) == 3 })
}

func TestTunnelReconnects(t *testing.T) {
	s := newFakeServer(t)
	tun := dial(t, s)

	// The server forgets the connection.
	s.send(&knxnet.DisconnectRequest{Channel: 1})

	eventually(t, "the disconnect response", func() bool { return s.count(&knxnet.DisconnectResponse{}) == 1 })

	if err := tun.Send(groupWrite(1)); !errors.Is(err, ErrDisconnected) {
		t.Errorf("Send while disconnected: %v", err)
	}

	eventually(t, "a new connection", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()

		return s.connections == 2
	})

	eventually(t, "a working tunnel", func() bool { return tun.Send(groupWrite(1)) == nil })
}

func TestTunnelLostAcks(t *testing.T) {
	s := newFakeServer(t)
	tun := dial(t, s)

	s.mu.Lock()
	s.ignoreAcks = true
	s.mu.Unlock()

	if err := tun.Send(groupWrite(1)); !errors.Is(err, ErrDisconnected) {
		t.Errorf("Send without acks: %v", err)
	}

	if n := s.count(&knxnet.TunnellingRequest{}); n != 2 {
		t.Errorf("Request sent %d times, expected 2", n)
	}
}

// routerPair returns two routers that send to each other over localhost UDP.
func routerPair(t *testing.T) (a, b *Router) {
	t.Helper()

	connA, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}

	connB, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}

	a = NewRouter(connA, connB.LocalAddr(), RouterConfig{Address: cemi.NewIndividualAddr3(1, 0, 1)})
	b = NewRouter(connB, connA.LocalAddr(), RouterConfig{Address: cemi.NewIndividualAddr3(1, 0, 2)})

	t.Cleanup(func() {
		a.Close()
		b.Close()
	})

	return a, b
}

func TestRouter(t *testing.T) {
	a, b := routerPair(t)

	if err := a.Send(groupWrite(1)); err != nil {
		t.Fatal(err)
	}

	select {
	case ind := <-b.Inbound():
		if ind.Source != a.Address() || ind.Destination != 0x0901 {
			t.Errorf("Received %+v", ind.LData)
		}
	case <-time.After(time.Second):
		t.Fatal("Nothing received")
	}

	// Frames with our own address are our own, looped back by multicast.
	b.SetAddress(a.Address())
	a.Send(groupWrite(2))

	select {
	case ind := <-b.Inbound():
		t.Errorf("Received own frame %+v", ind.LData)
	case <-time.After(50 * time.Millisecond):
	}

	a.Close()

	if _, open := <-a.Inbound(); open {
		t.Error("Inbound still open after Close")
	}

	if err := a.Send(groupWrite(1)); !errors.Is(err, link.ErrClosed) {
		t.Errorf("Send after Close: %v", err)
	}
}

func TestRouterBusy(t *testing.T) {
	a, b := routerPair(t)

	// b's peer is a: tell a to pause for 100 ms.
	b.tx.WriteTo(knxnet.Encode(&knxnet.RoutingBusy{WaitTime: 100}), b.group)
	b.tx.WriteTo(knxnet.Encode(&knxnet.RoutingLostMessage{Lost: 3}), b.group)

	eventually(t, "the lost message count", func() bool { return a.Lost() == 3 })

	start := time.Now()
	a.Send(groupWrite(1))

	if elapsed := time.Since(start); elapsed < 80*time.Millisecond {
		t.Errorf("Send did not pause: %v", elapsed)
	}

	// Consecutive frames keep the minimum interval.
	start = time.Now()
	for i := 0; i < 3; i++ {
		a.Send(groupWrite(1))
	}

	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Errorf("3 frames sent in %v", elapsed)
	}
}

// TestRouterMulticast uses the real routing group. It needs a network interface with multicast.
func TestRouterMulticast(t *testing.T) {
	a, err := ListenRouter(nil, RouterConfig{Address: cemi.NewIndividualAddr3(1, 0, 1)})
	if err != nil {
		t.Skipf("No multicast: %v", err)
	}
	defer a.Close()

	b, err := ListenRouter(nil, RouterConfig{Address: cemi.NewIndividualAddr3(1, 0, 2)})
	if err != nil {
		t.Skipf("No multicast: %v", err)
	}
	defer b.Close()

	if err := a.Send(groupWrite(1)); err != nil {
		t.Skipf("Cannot send to the routing group: %v", err)
	}

	select {
	case ind := <-b.Inbound():
		if ind.Source != a.Address() {
			t.Errorf("Received %+v", ind.LData)
		}
	case <-time.After(time.Second):
		t.Skip("Multicast loopback does not work here")
	}
}

func TestSearch(t *testing.T) {
	responder, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer responder.Close()

	info := knxnet.DeviceInfo{
		Medium:   0x02,
		Address:  cemi.NewIndividualAddr3(1, 1, 0),
		Name:     "test router",
		Families: []knxnet.ServiceFamily{{ID: knxnet.FamilyTunnelling, Version: 1}},
	}

	go func() {
		buffer := make([]byte, 512)

		n, _, err := responder.ReadFromUDP(buffer)
		if err != nil {
			return
		}

		s, err := knxnet.Decode(buffer[:n])
		request, ok := s.(*knxnet.SearchRequest)
		if err != nil || !ok {
			t.Errorf("Expected a search request, got %x", buffer[:n])
			return
		}

		// Answer to the discovery endpoint: once with a route-back control HPAI, then again with
		// the same endpoint spelled out, which must not be listed twice.
		to := &net.UDPAddr{IP: net.IP(request.Discovery.IP[:]), Port: int(request.Discovery.Port)}
		self := responder.LocalAddr().(*net.UDPAddr)

		responder.WriteToUDP(knxnet.Encode(&knxnet.SearchResponse{Device: info}), to)

		explicit := &knxnet.SearchResponse{Device: info, Control: knxnet.HPAI{Port: uint16(self.Port)}}
		copy(explicit.Control.IP[:], self.IP.To4())
		responder.WriteToUDP(knxnet.Encode(explicit), to)
	}()

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	servers, err := search(conn, responder.LocalAddr().(*net.UDPAddr), 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}

	if len(servers) != 1 || servers[0].Control.String() != responder.LocalAddr().String() ||
		servers[0].Info.Name != "test router" || !servers[0].Info.Supports(knxnet.FamilyTunnelling) {
		t.Errorf("Found %+v", servers)
	}
}
