package knxip

import (
	"bytes"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/shabunin/tiny-knx-go/cemi"
	"github.com/shabunin/tiny-knx-go/knxnet"
	"github.com/shabunin/tiny-knx-go/link"
	"github.com/shabunin/tiny-knx-go/link/mock"
)

// startObjectServer serves tunnels on bus and an ObjectServer that answers each request with the
// request after 0xAA, and closes the connection on the request "close".
func startObjectServer(t *testing.T, bus *mock.Bus) (*TunnelServer, string) {
	t.Helper()

	control, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}

	s := NewTunnelServer(control, nil, TunnelServerConfig{
		Server: knxnet.ServerConfig{Tunnels: serverTunnels, ObjectServer: 0x20},
		Attach: func(addr cemi.IndividualAddr) link.Link { return bus.Attach(addr) },
		ObjectServer: func(conn *ObjectServerConn) {
			for req := range conn.Requests() {
				if string(req) == "close" {
					conn.Close()
					continue
				}

				conn.Send(append([]byte{0xAA}, req...))
			}
		},
		Log: t.Logf,
	})

	t.Cleanup(func() { s.Close() })

	return s, dialAddress(t, s)
}

func receiveMessage(t *testing.T, ch <-chan []byte) []byte {
	t.Helper()

	select {
	case m := <-ch:
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("Nothing received")
		return nil
	}
}

func TestObjectServer(t *testing.T) {
	bus := mock.NewBus()
	dev := bus.Attach(busDevice)
	_, address := startObjectServer(t, bus)

	a, err := DialObjectServer(address, testConfig.Tunnel)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	b, err := DialObjectServer(address, testConfig.Tunnel)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	// ObjectServer connections take no tunnel: a tunnel still gets the first address.
	tun, err := DialTunnel(address, testConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()

	if tun.Address() != serverTunnels[0] {
		t.Errorf("Tunnel address %v", tun.Address())
	}

	for i, c := range []*ObjectServerClient{a, b, a} {
		req := []byte{0xF0, 0x01, 0x00, byte(i)}
		if err := c.Send(req); err != nil {
			t.Fatal(err)
		}

		if got := receiveMessage(t, c.Inbound()); !bytes.Equal(got, append([]byte{0xAA}, req...)) {
			t.Fatalf("Response % x to % x", got, req)
		}
	}

	// The tunnel works alongside.
	if err := tun.Send(groupWrite(1)); err != nil {
		t.Fatal(err)
	}

	receive(t, dev.Inbound())

	// The ObjectServer closes a connection: the client's inbound messages end.
	if err := a.Send([]byte("close")); err != nil {
		t.Fatal(err)
	}

	select {
	case m, ok := <-a.Inbound():
		if ok {
			t.Fatalf("Received % x", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("The connection stayed open")
	}

	if err := a.Send([]byte{0xF0}); !errors.Is(err, link.ErrClosed) {
		t.Errorf("Send after close: %v", err)
	}

	if err := b.Send([]byte{0xF0}); err != nil {
		t.Errorf("Other connection: %v", err)
	}
}

func TestObjectServerRefused(t *testing.T) {
	// A server without ObjectServer refuses the connection.
	_, err := DialObjectServer(dialAddress(t, startTunnelServer(t, mock.NewBus())), testConfig.Tunnel)
	if !errors.Is(err, knxnet.StatusConnectionType) {
		t.Fatalf("Dial: %v", err)
	}
}

func TestObjectServerSearch(t *testing.T) {
	_, address := startObjectServer(t, mock.NewBus())

	udp, _ := net.ResolveUDPAddr("udp4", address)
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	servers, err := search(conn, udp, 500*time.Millisecond)
	if err != nil || len(servers) != 1 {
		t.Fatalf("Servers %+v, %v", servers, err)
	}

	if v, ok := servers[0].Info.ObjectServer(); !ok || v != 0x20 || !servers[0].Info.Supports(knxnet.FamilyObjectServer) {
		t.Errorf("Device %+v", servers[0].Info)
	}
}

func dialAddress(t *testing.T, s *TunnelServer) string {
	t.Helper()

	c := s.Control()

	return (&net.UDPAddr{IP: net.IP(c.IP[:]), Port: int(c.Port)}).String()
}
