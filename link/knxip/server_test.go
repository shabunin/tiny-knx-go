package knxip

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/shabunin/tiny-knx-go/cemi"
	"github.com/shabunin/tiny-knx-go/knxnet"
	"github.com/shabunin/tiny-knx-go/link"
	"github.com/shabunin/tiny-knx-go/link/mock"
)

var (
	serverTunnels = []cemi.IndividualAddr{cemi.NewIndividualAddr3(1, 1, 250), cemi.NewIndividualAddr3(1, 1, 251)}
	busDevice     = cemi.NewIndividualAddr3(1, 1, 10)
)

func startTunnelServer(t *testing.T, bus *mock.Bus) *TunnelServer {
	t.Helper()

	control, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}

	s := NewTunnelServer(control, nil, TunnelServerConfig{
		Server: knxnet.ServerConfig{
			Device: knxnet.DeviceInfo{
				Medium:       0x02,
				Address:      cemi.NewIndividualAddr3(1, 1, 0),
				SerialNumber: [6]byte{0x00, 0xfa, 1, 2, 3, 4},
				Name:         "virtual bus",
			},
			Tunnels: serverTunnels,
		},
		Attach: func(addr cemi.IndividualAddr) link.Link { return bus.Attach(addr) },
		Log:    t.Logf,
	})

	t.Cleanup(func() { s.Close() })

	return s
}

func dialServer(t *testing.T, s *TunnelServer) *Tunnel {
	t.Helper()

	c := s.Control()
	addr := &net.UDPAddr{IP: net.IP(c.IP[:]), Port: int(c.Port)}

	tun, err := DialTunnel(addr.String(), testConfig)
	if err != nil {
		t.Fatalf("DialTunnel: %v", err)
	}

	t.Cleanup(func() { tun.Close() })

	return tun
}

func receive(t *testing.T, ch <-chan *cemi.LDataInd) *cemi.LDataInd {
	t.Helper()

	select {
	case ind := <-ch:
		return ind
	case <-time.After(2 * time.Second):
		t.Fatal("Nothing received")
		return nil
	}
}

func TestTunnelServer(t *testing.T) {
	bus := mock.NewBus()
	dev := bus.Attach(busDevice)
	s := startTunnelServer(t, bus)

	a := dialServer(t, s)
	b := dialServer(t, s)

	if a.Address() != serverTunnels[0] || b.Address() != serverTunnels[1] {
		t.Fatalf("Tunnel addresses %v and %v", a.Address(), b.Address())
	}

	// A third client finds no free tunnel.
	c := s.Control()
	if _, err := DialTunnel((&net.UDPAddr{IP: net.IP(c.IP[:]), Port: int(c.Port)}).String(), testConfig); !errors.Is(err, knxnet.StatusNoMoreConnections) {
		t.Errorf("Third tunnel: %v", err)
	}

	// Group frames go to the bus and to the other tunnel.
	if err := a.Send(groupWrite(1)); err != nil {
		t.Fatal(err)
	}

	for _, ch := range []<-chan *cemi.LDataInd{dev.Inbound(), b.Inbound()} {
		if ind := receive(t, ch); ind.Source != serverTunnels[0] || ind.Destination != 0x0901 {
			t.Errorf("Received %+v", ind)
		}
	}

	// Individual frames are confirmed only if the destination exists, and reach only it.
	req := &cemi.LDataReq{LData: cemi.LData{
		Control1:    cemi.Control1Request(cemi.PrioLow),
		Control2:    cemi.Control2Hops(6),
		Destination: uint16(busDevice),
		Data:        &cemi.ControlData{Command: 0}, // T_Connect,
	}}

	if err := a.Send(req); err != nil {
		t.Fatal(err)
	}

	receive(t, dev.Inbound())

	absent := *req
	absent.Destination = uint16(cemi.NewIndividualAddr3(1, 1, 99))

	if err := a.Send(&absent); !errors.Is(err, link.ErrNotConfirmed) {
		t.Errorf("Frame to an absent device: %v", err)
	}

	// The bus device answers a tunnel; the other tunnel does not see it.
	reply := *req
	reply.Destination = uint16(serverTunnels[0])
	dev.Send(&reply)

	if ind := receive(t, a.Inbound()); ind.Source != busDevice {
		t.Errorf("Received %+v", ind)
	}

	dev.Send(&cemi.LDataReq{LData: groupWrite(2).LData})

	if ind := receive(t, b.Inbound()); ind.Source != busDevice || ind.Data.(*cemi.AppData).Data[0] != 2 {
		t.Errorf("Tunnel b received %+v", ind)
	}

	// A closed tunnel leaves the bus and its address is free again.
	a.Close()
	eventually(t, "tunnel to close", func() bool {
		return dev.Send(&reply) != nil
	})

	if again := dialServer(t, s); again.Address() != serverTunnels[0] {
		t.Errorf("Reconnected with %v", again.Address())
	}
}

func TestTunnelServerManagement(t *testing.T) {
	s := startTunnelServer(t, mock.NewBus())

	c := s.Control()
	server := &net.UDPAddr{IP: net.IP(c.IP[:]), Port: int(c.Port)}

	conn, err := net.DialUDP("udp4", nil, server)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	exchange := func(request knxnet.Service, expect func(knxnet.Service) bool) knxnet.Service {
		t.Helper()

		if request != nil {
			conn.Write(knxnet.Encode(request))
		}

		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		buffer := make([]byte, 512)

		for {
			n, err := conn.Read(buffer)
			if err != nil {
				t.Fatalf("No answer to %T: %v", request, err)
			}

			if s, err := knxnet.Decode(buffer[:n]); err == nil && expect(s) {
				return s
			}
		}
	}

	r := exchange(&knxnet.DescriptionRequest{}, func(s knxnet.Service) bool { _, ok := s.(*knxnet.DescriptionResponse); return ok })
	if d := r.(*knxnet.DescriptionResponse).Device; d.Name != "virtual bus" || !d.Supports(knxnet.FamilyDeviceManagement) {
		t.Errorf("Description %+v", d)
	}

	r = exchange(&knxnet.ConnectRequest{Type: knxnet.DeviceManagementConnection}, func(s knxnet.Service) bool { _, ok := s.(*knxnet.ConnectResponse); return ok })

	channel := r.(*knxnet.ConnectResponse).Channel

	reads := []struct {
		prop  cemi.MProp
		count uint8
		data  []byte
	}{
		{cemi.MProp{ObjectType: 0, PropertyID: 11, Count: 1, Start: 1}, 1, []byte{0x00, 0xfa, 1, 2, 3, 4}},
		{cemi.MProp{ObjectType: 11, PropertyID: 53, Count: 1, Start: 0}, 1, []byte{0, 2}},
		{cemi.MProp{ObjectType: 11, PropertyID: 53, Count: 2, Start: 1}, 2, []byte{0x11, 250, 0x11, 251}},
		{cemi.MProp{ObjectType: 8, PropertyID: 52, Count: 1, Start: 1}, 1, []byte{0}},
		{cemi.MProp{ObjectType: 0, PropertyID: 200, Count: 1, Start: 1}, 0, []byte{0x07}},
	}

	for i, read := range reads {
		request := &knxnet.DeviceConfigRequest{Channel: channel, Sequence: uint8(i), Message: &cemi.MPropReadReq{MProp: read.prop}}

		exchange(request, func(s knxnet.Service) bool { a, ok := s.(*knxnet.DeviceConfigAck); return ok && a.Sequence == uint8(i) })

		r := exchange(nil, func(s knxnet.Service) bool { _, ok := s.(*knxnet.DeviceConfigRequest); return ok })
		indication := r.(*knxnet.DeviceConfigRequest)
		conn.Write(knxnet.Encode(&knxnet.DeviceConfigAck{Channel: channel, Sequence: indication.Sequence}))

		con, ok := indication.Message.(*cemi.MPropReadCon)
		if !ok || con.Count != read.count || string(con.Data) != string(read.data) {
			t.Errorf("Read %+v: %+v", read.prop, indication.Message)
		}
	}
}
