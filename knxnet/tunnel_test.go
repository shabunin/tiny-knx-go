package knxnet

import (
	"errors"
	"testing"
	"time"

	"github.com/shabunin/tiny-knx-go/cemi"
)

var (
	t0         = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	tunnelAddr = cemi.NewIndividualAddr3(1, 1, 250)
)

// sent is what a Tunnel asked to send, decoded.
type sent struct {
	service Service
	toData  bool
}

// drain returns the events of a tunnel, with EventSend packets decoded.
func drain(t *testing.T, tun *Tunnel) (packets []sent, events []Event) {
	t.Helper()

	for {
		event, ok := tun.NextEvent()
		if !ok {
			return packets, events
		}

		if event.Kind != EventSend {
			events = append(events, event)
			continue
		}

		s, err := Decode(event.Packet)
		if err != nil {
			t.Fatalf("Tunnel sent an invalid packet %x: %v", event.Packet, err)
		}

		packets = append(packets, sent{s, event.ToData})
	}
}

func connected(t *testing.T) *Tunnel {
	t.Helper()

	tun := NewTunnel(TunnelConfig{})
	tun.Connect(t0)

	packets, _ := drain(t, tun)
	if len(packets) != 1 || packets[0].toData {
		t.Fatalf("Expected a connect request to the control endpoint, got %+v", packets)
	}

	request, ok := packets[0].service.(*ConnectRequest)
	if !ok || request.Type != TunnelConnection || request.Layer != TunnelLinkLayer ||
		!request.Control.IsRouteBack() || !request.Data.IsRouteBack() {
		t.Fatalf("Bad connect request %+v", packets[0].service)
	}

	tun.HandlePacket(t0, Encode(&ConnectResponse{
		Channel: 7, Type: TunnelConnection, Address: tunnelAddr,
		Data: HPAI{ProtocolUDP, [4]byte{10, 0, 0, 2}, 3672},
	}))

	_, events := drain(t, tun)
	if len(events) != 1 || events[0].Kind != EventConnected || events[0].Address != tunnelAddr {
		t.Fatalf("Expected TunnelConnected, got %+v", events)
	}

	if tun.DataEndpoint().Port != 3672 || tun.Address() != tunnelAddr {
		t.Errorf("Data endpoint %+v, address %v", tun.DataEndpoint(), tun.Address())
	}

	return tun
}

func expectClosed(t *testing.T, tun *Tunnel, events []Event, err error) {
	t.Helper()

	if len(events) != 1 || events[0].Kind != EventDisconnected || !errors.Is(events[0].Err, err) {
		t.Fatalf("Expected EventDisconnected(%v), got %+v", err, events)
	}

	if tun.State() != TunnelClosed {
		t.Errorf("State %d", tun.State())
	}
}

func groupWrite() *cemi.LDataReq {
	return &cemi.LDataReq{LData: cemi.LData{
		Control1:    0xBC,
		Control2:    0xE0,
		Destination: 0x0901,
		Data:        &cemi.AppData{Command: cemi.GroupValueWrite, Data: []byte{1}},
	}}
}

func TestTunnelConnectRejected(t *testing.T) {
	tun := NewTunnel(TunnelConfig{})
	tun.Connect(t0)
	drain(t, tun)

	tun.HandlePacket(t0, Encode(&ConnectResponse{Channel: 0, Status: StatusNoMoreConnections}))

	_, events := drain(t, tun)
	expectClosed(t, tun, events, StatusNoMoreConnections)
}

func TestTunnelConnectTimeout(t *testing.T) {
	tun := NewTunnel(TunnelConfig{})
	tun.Connect(t0)
	drain(t, tun)

	if deadline, ok := tun.Deadline(); !ok || !deadline.Equal(t0.Add(10*time.Second)) {
		t.Fatalf("Deadline %v %v", deadline, ok)
	}

	tun.Tick(t0.Add(10 * time.Second))

	_, events := drain(t, tun)
	expectClosed(t, tun, events, ErrConnectTimeout)
}

func TestTunnelSend(t *testing.T) {
	tun := connected(t)

	for seq := 0; seq < 3; seq++ {
		if err := tun.Send(t0, groupWrite()); err != nil {
			t.Fatal(err)
		}

		if err := tun.Send(t0, groupWrite()); !errors.Is(err, ErrBusy) {
			t.Errorf("Second Send: %v", err)
		}

		packets, _ := drain(t, tun)
		request, ok := packets[0].service.(*TunnellingRequest)

		if len(packets) != 1 || !packets[0].toData || !ok || request.Channel != 7 ||
			request.Sequence != uint8(seq) {
			t.Fatalf("Expected tunnelling request %d, got %+v", seq, packets)
		}

		// A wrong sequence number is ignored.
		tun.HandlePacket(t0, Encode(&TunnellingAck{Channel: 7, Sequence: uint8(seq + 1)}))
		tun.HandlePacket(t0, Encode(&TunnellingAck{Channel: 7, Sequence: uint8(seq)}))

		_, events := drain(t, tun)
		if len(events) != 1 || events[0].Kind != EventAcked || events[0].Err != nil {
			t.Fatalf("Expected EventAcked, got %+v", events)
		}
	}

	// A rejected request keeps its sequence number.
	tun.Send(t0, groupWrite())
	drain(t, tun)
	tun.HandlePacket(t0, Encode(&TunnellingAck{Channel: 7, Sequence: 3, Status: StatusKNXConnection}))

	if _, events := drain(t, tun); len(events) != 1 || events[0].Err != StatusKNXConnection {
		t.Fatalf("Expected rejection, got %+v", events)
	}

	tun.Send(t0, groupWrite())

	if packets, _ := drain(t, tun); packets[0].service.(*TunnellingRequest).Sequence != 3 {
		t.Error("Sequence number advanced after a rejection")
	}
}

func TestTunnelSendRepeatsOnce(t *testing.T) {
	tun := connected(t)
	tun.Send(t0, groupWrite())
	drain(t, tun)

	tun.Tick(t0.Add(time.Second))

	packets, _ := drain(t, tun)
	if len(packets) != 1 || packets[0].service.(*TunnellingRequest).Sequence != 0 {
		t.Fatalf("Expected the request again, got %+v", packets)
	}

	tun.Tick(t0.Add(2 * time.Second))

	packets, events := drain(t, tun)
	if len(packets) != 1 {
		t.Fatalf("Expected a disconnect request, got %+v", packets)
	}

	if _, ok := packets[0].service.(*DisconnectRequest); !ok || packets[0].toData {
		t.Errorf("Expected a disconnect request to the control endpoint, got %+v", packets[0])
	}

	expectClosed(t, tun, events, ErrAckTimeout)

	if err := tun.Send(t0, groupWrite()); !errors.Is(err, ErrNotConnected) {
		t.Errorf("Send on a closed tunnel: %v", err)
	}
}

func TestTunnelReceive(t *testing.T) {
	tun := connected(t)

	indication := func(seq uint8, channel uint8) []byte {
		return Encode(&TunnellingRequest{Channel: channel, Sequence: seq, Message: &cemi.LDataInd{
			LData: groupWrite().LData,
		}})
	}

	expect := func(what string, acks []uint8, messages int) {
		t.Helper()

		packets, events := drain(t, tun)

		if len(packets) != len(acks) || len(events) != messages {
			t.Fatalf("%s: got %d packets and %d events", what, len(packets), len(events))
		}

		for i, p := range packets {
			ack, ok := p.service.(*TunnellingAck)
			if !ok || !p.toData || ack.Sequence != acks[i] || ack.Channel != 7 || ack.Status != StatusOK {
				t.Errorf("%s: expected ack %d, got %+v", what, acks[i], p)
			}
		}

		for _, e := range events {
			if _, ok := e.Message.(*cemi.LDataInd); e.Kind != EventMessage || !ok {
				t.Errorf("%s: expected an L_Data.ind, got %+v", what, e)
			}
		}
	}

	tun.HandlePacket(t0, indication(0, 7))
	expect("first", []uint8{0}, 1)

	tun.HandlePacket(t0, indication(0, 7))
	expect("repeated", []uint8{0}, 0)

	tun.HandlePacket(t0, indication(5, 7))
	expect("out of order", nil, 0)

	tun.HandlePacket(t0, indication(1, 8))
	expect("other channel", nil, 0)

	tun.HandlePacket(t0, indication(1, 7))
	expect("second", []uint8{1}, 1)
}

func TestTunnelHeartbeat(t *testing.T) {
	tun := connected(t)
	beat := t0.Add(60 * time.Second)

	if deadline, _ := tun.Deadline(); !deadline.Equal(beat) {
		t.Fatalf("First heartbeat at %v", deadline)
	}

	tun.Tick(beat)

	packets, _ := drain(t, tun)
	if _, ok := packets[0].service.(*ConnectionStateRequest); len(packets) != 1 || !ok || packets[0].toData {
		t.Fatalf("Expected a connection state request, got %+v", packets)
	}

	tun.HandlePacket(beat, Encode(&ConnectionStateResponse{Channel: 7}))

	if deadline, _ := tun.Deadline(); !deadline.Equal(beat.Add(60 * time.Second)) {
		t.Fatalf("Next heartbeat at %v", deadline)
	}

	// Three unanswered requests close the tunnel.
	now := beat.Add(60 * time.Second)
	for i := 0; i < 3; i++ {
		tun.Tick(now)

		if packets, _ := drain(t, tun); len(packets) != 1 {
			t.Fatalf("Attempt %d: %+v", i, packets)
		}

		now = now.Add(10 * time.Second)
	}

	tun.Tick(now)

	_, events := drain(t, tun)
	expectClosed(t, tun, events, ErrHeartbeatTimeout)
}

func TestTunnelHeartbeatError(t *testing.T) {
	tun := connected(t)
	tun.Tick(t0.Add(60 * time.Second))
	drain(t, tun)

	tun.HandlePacket(t0, Encode(&ConnectionStateResponse{Channel: 7, Status: StatusConnectionID}))

	_, events := drain(t, tun)
	expectClosed(t, tun, events, StatusConnectionID)
}

func TestTunnelDisconnect(t *testing.T) {
	tun := connected(t)
	tun.Disconnect(t0)

	packets, events := drain(t, tun)
	if _, ok := packets[0].service.(*DisconnectRequest); len(packets) != 1 || !ok || len(events) != 0 {
		t.Fatalf("Expected a disconnect request, got %+v", packets)
	}

	tun.HandlePacket(t0, Encode(&DisconnectResponse{Channel: 7}))

	_, events = drain(t, tun)
	expectClosed(t, tun, events, nil)

	// Without a response, Disconnect gives up after the timeout.
	tun = connected(t)
	tun.Disconnect(t0)
	drain(t, tun)
	tun.Tick(t0.Add(2 * time.Second))

	_, events = drain(t, tun)
	expectClosed(t, tun, events, nil)
}

func TestTunnelClosedByServer(t *testing.T) {
	tun := connected(t)

	tun.HandlePacket(t0, Encode(&DisconnectRequest{Channel: 8}))
	if packets, events := drain(t, tun); len(packets)+len(events) != 0 {
		t.Fatal("Reacted to a disconnect request for another channel")
	}

	tun.HandlePacket(t0, Encode(&DisconnectRequest{Channel: 7}))

	packets, events := drain(t, tun)
	if response, ok := packets[0].service.(*DisconnectResponse); len(packets) != 1 || !ok || response.Channel != 7 {
		t.Fatalf("Expected a disconnect response, got %+v", packets)
	}

	expectClosed(t, tun, events, ErrClosedByServer)

	// The tunnel can connect again.
	tun.Connect(t0)

	if packets, _ := drain(t, tun); len(packets) != 1 || tun.State() != TunnelConnecting {
		t.Errorf("Reconnect sent %+v", packets)
	}
}
