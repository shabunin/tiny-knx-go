package knxnet

import (
	"bytes"
	"testing"

	"github.com/shabunin/tiny-knx-go/cemi"
)

// GetServerItem.Req for the hardware type, and its response, from the ObjectServer protocol.
var (
	getHardwareType = []byte{0xF0, 0x01, 0x00, 0x01, 0x00, 0x01}
	hardwareType    = []byte{0xF0, 0x81, 0x00, 0x01, 0x00, 0x01, 0x00, 0x01, 0x06, 0x00, 0x00, 0xC5, 0x07, 0x00, 0x02}
)

func TestObjectServerPackets(t *testing.T) {
	// The frame of the example in the ObjectServer protocol, which has channel and sequence 0.
	want := append([]byte{0x06, 0x20, 0xF0, 0x80, 0x00, 0x10, 0x04, 0x00, 0x00, 0x00}, getHardwareType...)
	if got := Encode(&ObjectServerRequest{Message: getHardwareType}); !bytes.Equal(got, want) {
		t.Fatalf("Request % x, want % x", got, want)
	}

	s, err := Decode(Encode(&ObjectServerRequest{Channel: 7, Sequence: 3, Message: hardwareType}))
	if r, ok := s.(*ObjectServerRequest); err != nil || !ok || r.Channel != 7 || r.Sequence != 3 || !bytes.Equal(r.Message, hardwareType) {
		t.Fatalf("Decoded %+v, %v", s, err)
	}

	ack := Encode(&ObjectServerAck{Channel: 7, Sequence: 3, Status: StatusOK})
	if want := []byte{0x06, 0x20, 0xF0, 0x81, 0x00, 0x0A, 0x04, 0x07, 0x03, 0x00}; !bytes.Equal(ack, want) {
		t.Fatalf("Ack % x, want % x", ack, want)
	}

	// Version 1.0 in the header is taken too, but not for the other services.
	ack[1] = 0x10
	if s, err := Decode(ack); err != nil {
		t.Fatalf("Decoded %+v, %v", s, err)
	}

	if _, err := Decode(Encode(&ConnectionStateResponse{})[:1:1]); err == nil {
		t.Fatal("Decoded a truncated header")
	}

	state := Encode(&ConnectionStateResponse{})
	state[1] = 0x20
	if _, err := Decode(state); err != ErrHeader {
		t.Fatalf("Decoded version 2.0 of a core service: %v", err)
	}
}

func searchResponse(t *testing.T, server *Server) []byte {
	t.Helper()

	server.HandlePacket(t0, clientEndpoint, Encode(&SearchRequest{Discovery: clientEndpoint}))

	event, ok := server.NextEvent()
	if !ok || event.Kind != ServerSend {
		t.Fatalf("Event %+v", event)
	}

	return event.Packet
}

func TestObjectServerSearch(t *testing.T) {
	// Without tunnels, the response has the layout of the ObjectServer protocol: 84 bytes with
	// the manufacturer data at 76.
	packet := searchResponse(t, NewServer(ServerConfig{Control: serverEndpoint, ObjectServer: 0x20}))

	if len(packet) != 84 {
		t.Fatalf("Response of %d bytes: % x", len(packet), packet)
	}

	if want := []byte{0x08, 0xFE, 0x00, 0xC5, 0x01, 0x04, 0xF0, 0x20}; !bytes.Equal(packet[76:], want) {
		t.Fatalf("Manufacturer DIB % x, want % x", packet[76:], want)
	}

	s, err := Decode(packet)
	if err != nil {
		t.Fatal(err)
	}

	info := s.(*SearchResponse).Device
	if v, ok := info.ObjectServer(); !ok || v != 0x20 {
		t.Errorf("ObjectServer %#x, %v", v, ok)
	}

	if !info.Supports(FamilyObjectServer) || info.Supports(FamilyTunnelling) {
		t.Errorf("Families %+v", info.Families)
	}

	// With tunnels, an IP BAOS is an interface too.
	s, _ = Decode(searchResponse(t, NewServer(ServerConfig{Control: serverEndpoint, Tunnels: []cemi.IndividualAddr{tunnelAddr}, ObjectServer: 0x10})))
	info = s.(*SearchResponse).Device

	if v, ok := info.ObjectServer(); !ok || v != 0x10 || !info.Supports(FamilyTunnelling) || !info.Supports(FamilyObjectServer) {
		t.Errorf("ObjectServer %#x, %v, families %+v", v, ok, info.Families)
	}

	// A plain server has no manufacturer data.
	s, _ = Decode(searchResponse(t, NewServer(ServerConfig{Control: serverEndpoint, Tunnels: []cemi.IndividualAddr{tunnelAddr}})))
	info = s.(*SearchResponse).Device

	if _, ok := info.ObjectServer(); ok || info.ManufacturerData != nil || info.Supports(FamilyObjectServer) {
		t.Errorf("Device %+v", info)
	}
}

func TestObjectServerConnection(t *testing.T) {
	p := &pair{
		t:      t,
		server: NewServer(ServerConfig{Control: serverEndpoint, Tunnels: []cemi.IndividualAddr{tunnelAddr}, ObjectServer: 0x20}),
		client: NewTunnel(TunnelConfig{Type: ObjectServerConnection}),
		now:    t0,
	}

	p.client.Connect(p.now)
	p.pump()

	events, serverEvents := p.takeEvents()
	if len(events) != 1 || events[0].Kind != EventConnected || events[0].Address != 0 {
		t.Fatalf("Client events %+v", events)
	}

	if len(serverEvents) != 1 || serverEvents[0].Kind != ServerOpened || serverEvents[0].Type != ObjectServerConnection {
		t.Fatalf("Server events %+v", serverEvents)
	}

	channel := serverEvents[0].Channel

	// The connection takes no tunnel address.
	if conns := p.server.Connections(); conns[channel] != 0 {
		t.Fatalf("Connections %v", conns)
	}

	if err := p.client.SendObjectServer(p.now, getHardwareType); err != nil {
		t.Fatal(err)
	}

	p.pump()

	events, serverEvents = p.takeEvents()
	if len(events) != 1 || events[0].Kind != EventAcked || events[0].Err != nil {
		t.Fatalf("Client events %+v", events)
	}

	if len(serverEvents) != 1 || serverEvents[0].Kind != ServerMessage || serverEvents[0].Type != ObjectServerConnection ||
		serverEvents[0].Message != nil || !bytes.Equal(serverEvents[0].Data, getHardwareType) {
		t.Fatalf("Server events %+v", serverEvents)
	}

	// A response, then an indication: both wait for the acknowledgement of the one before.
	if !p.server.SendObjectServer(p.now, channel, hardwareType) || !p.server.SendObjectServer(p.now, channel, []byte{0xF0, 0xC1}) {
		t.Fatal("SendObjectServer failed")
	}

	if p.server.Indicate(p.now, channel, &cemi.LDataInd{LData: *serverFrame(0x1101)}) {
		t.Fatal("Indicated a cEMI message on an ObjectServer connection")
	}

	p.pump()

	events, _ = p.takeEvents()
	if len(events) != 2 || !bytes.Equal(events[0].Data, hardwareType) || !bytes.Equal(events[1].Data, []byte{0xF0, 0xC1}) {
		t.Fatalf("Client events %+v", events)
	}

	p.client.Disconnect(p.now)
	p.pump()

	if _, serverEvents = p.takeEvents(); len(serverEvents) != 1 || serverEvents[0].Kind != ServerClosed || serverEvents[0].Type != ObjectServerConnection {
		t.Fatalf("Server events %+v", serverEvents)
	}
}

func TestObjectServerRefused(t *testing.T) {
	// A server without ObjectServer refuses the connection type.
	p := newPair(t, tunnelAddr)
	p.client = NewTunnel(TunnelConfig{Type: ObjectServerConnection})
	p.client.Connect(p.now)
	p.pump()

	events, serverEvents := p.takeEvents()
	if len(events) != 1 || events[0].Kind != EventDisconnected || events[0].Err != StatusConnectionType || len(serverEvents) != 0 {
		t.Fatalf("Events %+v, %+v", events, serverEvents)
	}

	// Tunnelling requests on an ObjectServer connection are refused.
	p = &pair{
		t:      t,
		server: NewServer(ServerConfig{Control: serverEndpoint, ObjectServer: 0x20}),
		client: NewTunnel(TunnelConfig{Type: ObjectServerConnection}),
		now:    t0,
	}

	p.client.Connect(p.now)
	p.pump()
	p.takeEvents()

	p.server.HandlePacket(p.now, clientEndpoint, Encode(&TunnellingRequest{Channel: p.server.next, Message: &cemi.LDataReq{LData: *serverFrame(0)}}))

	event, _ := p.server.NextEvent()
	if s, _ := Decode(event.Packet); event.Kind != ServerSend || s.(*TunnellingAck).Status != StatusConnectionID {
		t.Fatalf("Event %+v", event)
	}
}
