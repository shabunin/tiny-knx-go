package knxnet

import (
	"testing"
	"time"

	"github.com/shabunin/tiny-knx-go/cemi"
)

var (
	serverEndpoint = HPAI{ProtocolUDP, [4]byte{10, 0, 0, 2}, Port}
	clientEndpoint = HPAI{ProtocolUDP, [4]byte{10, 0, 0, 5}, 50000}
)

// pair runs a Tunnel against a Server, delivering packets in memory.
type pair struct {
	t      *testing.T
	server *Server
	client *Tunnel
	now    time.Time

	drop         func(toClient bool, s Service) bool // loses a packet
	events       []Event                             // of the client, except EventSend
	serverEvents []ServerEvent                       // of the server, except ServerSend
}

func newPair(t *testing.T, tunnels ...cemi.IndividualAddr) *pair {
	return &pair{
		t:      t,
		server: NewServer(ServerConfig{Control: serverEndpoint, Tunnels: tunnels}),
		client: NewTunnel(TunnelConfig{}),
		now:    t0,
	}
}

// pump delivers packets until both sides are quiet.
func (p *pair) pump() {
	p.t.Helper()

	for busy := true; busy; {
		busy = false

		for {
			event, ok := p.client.NextEvent()
			if !ok {
				break
			}

			busy = true

			if event.Kind != EventSend {
				p.events = append(p.events, event)
				continue
			}

			if s, _ := Decode(event.Packet); p.drop == nil || !p.drop(false, s) {
				p.server.HandlePacket(p.now, clientEndpoint, event.Packet)
			}
		}

		for {
			event, ok := p.server.NextEvent()
			if !ok {
				break
			}

			busy = true

			if event.Kind != ServerSend {
				p.serverEvents = append(p.serverEvents, event)
				continue
			}

			if event.To != clientEndpoint {
				p.t.Fatalf("Server sent to %+v", event.To)
			}

			if s, _ := Decode(event.Packet); p.drop == nil || !p.drop(true, s) {
				p.client.HandlePacket(p.now, event.Packet)
			}
		}
	}
}

func (p *pair) advance(d time.Duration) {
	p.now = p.now.Add(d)
	p.client.Tick(p.now)
	p.server.Tick(p.now)
	p.pump()
}

func (p *pair) takeEvents() ([]Event, []ServerEvent) {
	c, s := p.events, p.serverEvents
	p.events, p.serverEvents = nil, nil

	return c, s
}

func serverFrame(source cemi.IndividualAddr) *cemi.LData {
	return &cemi.LData{
		Control1:    cemi.Control1StdFrame,
		Control2:    cemi.Control2Hops(6) | cemi.Control2GroupAddr,
		Source:      source,
		Destination: uint16(cemi.NewGroupAddr3(1, 2, 3)),
		Data:        &cemi.AppData{Command: cemi.GroupValueWrite, Data: []byte{1}},
	}
}

func connectPair(t *testing.T) *pair {
	t.Helper()

	p := newPair(t, tunnelAddr)
	p.client.Connect(p.now)
	p.pump()

	events, serverEvents := p.takeEvents()
	if len(events) != 1 || events[0].Kind != EventConnected || events[0].Address != tunnelAddr {
		t.Fatalf("Client events %+v", events)
	}

	if len(serverEvents) != 1 || serverEvents[0].Kind != ServerOpened || serverEvents[0].Address != tunnelAddr {
		t.Fatalf("Server events %+v", serverEvents)
	}

	if p.client.DataEndpoint() != serverEndpoint {
		t.Errorf("Data endpoint %+v", p.client.DataEndpoint())
	}

	return p
}

func TestServerTunnel(t *testing.T) {
	p := connectPair(t)

	// Client to bus: the request arrives with the tunnel address and is confirmed.
	if err := p.client.Send(p.now, &cemi.LDataReq{LData: *serverFrame(0)}); err != nil {
		t.Fatal(err)
	}

	p.pump()

	events, serverEvents := p.takeEvents()
	if len(events) != 1 || events[0].Kind != EventAcked || events[0].Err != nil {
		t.Fatalf("Client events %+v", events)
	}

	if len(serverEvents) != 1 || serverEvents[0].Kind != ServerMessage {
		t.Fatalf("Server events %+v", serverEvents)
	}

	req := serverEvents[0].Message.(*cemi.LDataReq)
	if req.Source != tunnelAddr {
		t.Errorf("Source %v", req.Source)
	}

	ch := serverEvents[0].Channel

	p.server.Confirm(p.now, ch, req, false)
	p.pump()

	events, _ = p.takeEvents()
	if len(events) != 1 || events[0].Kind != EventMessage {
		t.Fatalf("Client events %+v", events)
	}

	if con, ok := events[0].Message.(*cemi.LDataCon); !ok || con.Control1&cemi.Control1HasError == 0 {
		t.Errorf("Expected a negative confirmation, got %+v", events[0].Message)
	}

	// Bus to client, in order, with a lost acknowledgement on the way.
	lost := false
	p.drop = func(toClient bool, s Service) bool {
		if _, ok := s.(*TunnellingAck); ok && !toClient && !lost {
			lost = true
			return true
		}

		return false
	}

	for i := 1; i <= 3; i++ {
		if !p.server.Indicate(p.now, ch, &cemi.LDataInd{LData: *serverFrame(cemi.IndividualAddr(i))}) {
			t.Fatal("Indicate failed")
		}
	}

	p.pump()
	p.advance(time.Second)

	events, _ = p.takeEvents()
	if len(events) != 3 {
		t.Fatalf("Client events %+v", events)
	}

	for i, e := range events {
		if ind, ok := e.Message.(*cemi.LDataInd); !ok || ind.Source != cemi.IndividualAddr(i+1) {
			t.Errorf("Message %d: %+v", i, e.Message)
		}
	}

	// Disconnect from the client.
	p.client.Disconnect(p.now)
	p.pump()

	_, serverEvents = p.takeEvents()
	if len(serverEvents) != 1 || serverEvents[0].Kind != ServerClosed || len(p.server.Connections()) != 0 {
		t.Errorf("Server events %+v", serverEvents)
	}
}

func TestServerNoMoreTunnels(t *testing.T) {
	p := connectPair(t)

	other := NewTunnel(TunnelConfig{})
	other.Connect(p.now)

	event, _ := other.NextEvent()
	p.server.HandlePacket(p.now, clientEndpoint, event.Packet)

	reply, _ := p.server.NextEvent()
	other.HandlePacket(p.now, reply.Packet)

	event, _ = other.NextEvent()
	if event.Kind != EventDisconnected || event.Err != StatusNoMoreConnections {
		t.Errorf("Event %+v", event)
	}
}

func TestServerTimeouts(t *testing.T) {
	// A client that does not acknowledge loses its connection.
	p := connectPair(t)
	p.drop = func(toClient bool, s Service) bool {
		_, ok := s.(*TunnellingAck)
		return ok && !toClient
	}

	ch := byte(0)
	for c := range p.server.Connections() {
		ch = c
	}

	p.server.Indicate(p.now, ch, &cemi.LDataInd{LData: *serverFrame(1)})
	p.pump()
	p.advance(time.Second)
	p.advance(time.Second)

	events, serverEvents := p.takeEvents()
	if len(serverEvents) != 1 || serverEvents[0].Kind != ServerClosed {
		t.Errorf("Server events %+v", serverEvents)
	}

	if last := events[len(events)-1]; last.Kind != EventDisconnected || last.Err != ErrClosedByServer {
		t.Errorf("Client events %+v", events)
	}

	// A silent client loses its connection, one with heartbeats keeps it.
	p = connectPair(t)
	p.advance(100 * time.Second) // the client sends a heartbeat after 60 seconds

	if deadline, _ := p.server.Deadline(); !deadline.After(p.now) {
		t.Errorf("Deadline %v", deadline)
	}

	p.advance(100 * time.Second)

	if len(p.server.Connections()) != 1 {
		t.Error("Connection with heartbeats was closed")
	}

	p.client = NewTunnel(TunnelConfig{}) // forget the old one
	p.advance(121 * time.Second)

	if len(p.server.Connections()) != 0 {
		t.Error("Silent connection was not closed")
	}
}

func TestServerSearchAndDescription(t *testing.T) {
	s := NewServer(ServerConfig{Control: serverEndpoint, Device: DeviceInfo{Name: "vbus", Medium: 0x02}})

	discovery := HPAI{ProtocolUDP, [4]byte{10, 0, 0, 9}, 40000}

	for _, extended := range []bool{false, true} {
		s.HandlePacket(t0, clientEndpoint, Encode(&SearchRequest{Discovery: discovery, Extended: extended}))

		event, _ := s.NextEvent()
		if event.To != discovery {
			t.Errorf("Response to %+v", event.To)
		}

		r, err := Decode(event.Packet)
		response, ok := r.(*SearchResponse)

		if err != nil || !ok || response.Extended != extended || response.Control != serverEndpoint ||
			response.Device.Name != "vbus" || !response.Device.Supports(FamilyTunnelling) {
			t.Errorf("Search response %+v, %v", r, err)
		}
	}

	s.HandlePacket(t0, clientEndpoint, Encode(&DescriptionRequest{}))

	event, _ := s.NextEvent()
	if r, _ := Decode(event.Packet); event.To != clientEndpoint || r.(*DescriptionResponse).Device.Name != "vbus" {
		t.Errorf("Description response %+v to %+v", r, event.To)
	}

	// Management connections exchange device configuration requests.
	s.HandlePacket(t0, clientEndpoint, Encode(&ConnectRequest{Type: DeviceManagementConnection}))
	s.NextEvent()

	opened, _ := s.NextEvent()
	if opened.Kind != ServerOpened || opened.Address != 0 {
		t.Fatalf("Event %+v", opened)
	}

	read := &cemi.MPropReadReq{MProp: cemi.MProp{ObjectType: 0, PropertyID: 11, Count: 1, Start: 1}}
	s.HandlePacket(t0, clientEndpoint, Encode(&DeviceConfigRequest{Channel: opened.Channel, Message: read}))

	ack, _ := s.NextEvent()
	if r, _ := Decode(ack.Packet); r.(*DeviceConfigAck).Status != StatusOK {
		t.Errorf("Ack %+v", r)
	}

	if message, _ := s.NextEvent(); message.Kind != ServerMessage || message.Message.MessageCode() != cemi.MPropReadReqCode {
		t.Errorf("Event %+v", message)
	}

	// A tunnelling request on a management connection is rejected.
	s.HandlePacket(t0, clientEndpoint, Encode(&TunnellingRequest{Channel: opened.Channel, Sequence: 1, Message: read}))

	ack, _ = s.NextEvent()
	if r, _ := Decode(ack.Packet); r.(*TunnellingAck).Status != StatusConnectionID {
		t.Errorf("Ack %+v", r)
	}
}
