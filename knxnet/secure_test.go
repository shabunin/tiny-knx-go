package knxnet

import (
	"bytes"
	"crypto/aes"
	"crypto/ecdh"
	"testing"
	"time"

	"github.com/shabunin/tiny-knx-go/cemi"
	"github.com/shabunin/tiny-knx-go/secure"
)

// The handshake example of AN159, which xknx tests with: user 1 with the password "secret", the
// device authentication code "trustme", session 1.
var (
	an159Client = mustHex("b8 fa bd 62 66 5d 8b 9e 8a 9d 8b 1f 4b ca 42 c8 c2 78 9a 61 10 f5 0e 9d d7 85 b3 ed e8 83 f3 78")
	an159Public = mustHex("0a a2 27 b4 fd 7a 32 31 9b a9 96 0a c0 36 ce 0e 5c 45 07 b5 ae 55 16 1f 10 78 b1 dc fb 3c b6 31")
	an159Server = mustHex("bd f0 99 90 99 23 14 3e f0 a5 de 0b 3b e3 68 7b c5 bd 3c f5 f9 e6 f9 01 69 9c d8 70 ec 1f f8 24")
)

func key32(b []byte) *[PublicKeySize]byte {
	var k [PublicKeySize]byte
	copy(k[:], b)

	return &k
}

func TestSessionHandshake(t *testing.T) {
	deviceAuth := deviceKey("trustme")
	b, _ := aes.NewCipher(deviceAuth[:])

	mac := SessionResponseMAC(b, 1, key32(an159Public), key32(an159Server))
	if want := mustHex("a9 22 50 5a aa 43 61 63 57 0b d5 49 4c 2d f2 a3"); !bytes.Equal(mac[:], want) {
		t.Errorf("SessionResponseMAC = % x, want % x", mac, want)
	}

	password := userKey("secret")
	b, _ = aes.NewCipher(password[:])

	mac = SessionAuthenticateMAC(b, 1, key32(an159Public), key32(an159Server))
	if want := mustHex("1f 1d 59 ea 9f 12 a1 52 e5 d9 72 7f 08 46 2c de"); !bytes.Equal(mac[:], want) {
		t.Errorf("SessionAuthenticateMAC = % x, want % x", mac, want)
	}

	// The session key from the key exchange, and the SESSION_AUTHENTICATE in a SECURE_WRAPPER.
	curve := ecdh.X25519()
	private, _ := curve.NewPrivateKey(an159Client)
	if !bytes.Equal(private.PublicKey().Bytes(), an159Public) {
		t.Fatal("public key of the client differs")
	}

	server, _ := curve.NewPublicKey(an159Server)
	shared, _ := private.ECDH(server)
	sessionKey := secure.SessionKey(shared)
	key, _ := aes.NewCipher(sessionKey[:])

	authenticate := Encode(&SessionAuthenticate{User: 1, MAC: mac})
	n := secure.Nonce{Serial: [6]byte{0x00, 0xfa, 0x12, 0x34, 0x56, 0x78}, Tag: 0xaffe}

	packet := Wrap(key, 1, n, authenticate)
	want := mustHex("06 10 09 50 00 3e 00 01 00 00 00 00 00 00 00 fa 12 34 56 78 af fe" +
		"79 15 a4 f3 6e 6e 42 08 d2 8b 4a 20 7d 8f 35 c0 d1 38 c2 6a 7b 5e 71 69" +
		"52 db a8 e7 e4 bd 80 bd 7d 86 8a 3a e7 87 49 de")

	if !bytes.Equal(packet, want) {
		t.Errorf("Wrap = % x, want % x", packet, want)
	}

	// The SESSION_STATUS of the server.
	s, err := Decode(mustHex("06 10 09 50 00 2e 00 01 00 00 00 00 00 00 00 fa aa aa aa aa af fe" +
		"26 15 6d b5 c7 49 88 8f a3 73 c3 e0 b4 bd e4 49 7c 39 5e 4b 1c 2f 46 a1"))
	if err != nil {
		t.Fatal(err)
	}

	inner, err := s.(*SecureWrapper).Open(key)
	if err != nil {
		t.Fatal(err)
	}

	if status, err := Decode(inner); err != nil || *status.(*SessionStatus) != (SessionStatus{SessionAuthenticated}) {
		t.Errorf("SessionStatus = %+v, %v", status, err)
	}
}

// derived memoizes the keys of passwords, which take a while on microcontrollers.
var derived = map[string][16]byte{}

func userKey(password string) [16]byte {
	if k, ok := derived["user "+password]; ok {
		return k
	}

	k := secure.UserPasswordKey(password)
	derived["user "+password] = k

	return k
}

func deviceKey(code string) [16]byte {
	if k, ok := derived["device "+code]; ok {
		return k
	}

	k := secure.DeviceAuthKey(code)
	derived["device "+code] = k

	return k
}

// fixedRandom is a source of the same random bytes, for the key pairs.
type fixedRandom []byte

func (r fixedRandom) Read(b []byte) (int, error) {
	return copy(b, r), nil
}

func newSecurePair(t *testing.T, user uint8, password, deviceAuth string) *pair {
	p := &pair{
		t: t,
		server: NewServer(ServerConfig{
			Control: serverEndpoint,
			Tunnels: []cemi.IndividualAddr{tunnelAddr},
			Device:  DeviceInfo{SerialNumber: [6]byte{0x00, 0xfa, 0xaa, 0xaa, 0xaa, 0xaa}},
			Secure: &SecureConfig{
				DeviceAuth: deviceKey("trustme"),
				Users:      map[uint8][16]byte{1: userKey("secret"), 2: userKey("tunnel")},
				Random:     fixedRandom(mustHex("a8 fa bd 62 66 5d 8b 9e 8a 9d 8b 1f 4b ca 42 c8 c2 78 9a 61 10 f5 0e 9d d7 85 b3 ed e8 83 f3 78")),
			},
		}),
		client: NewTunnel(TunnelConfig{Secure: &TunnelSecure{
			User:     user,
			Password: userKey(password),
			Random:   fixedRandom(an159Client),
		}}),
		now: t0,
	}

	if deviceAuth != "" {
		p.client.config.Secure.DeviceAuth = deviceKey(deviceAuth)
	}

	p.client.Connect(p.now)
	p.pump()

	return p
}

func TestSecureTunnel(t *testing.T) {
	// Everything between client and server is wrapped, except the session request and response.
	plain := 0
	p := newSecurePair(t, 2, "tunnel", "trustme")
	p.drop = func(toClient bool, s Service) bool {
		switch s.(type) {
		case *SecureWrapper, *SessionRequest, *SessionResponse:
		default:
			plain++
		}

		return false
	}

	events, serverEvents := p.takeEvents()
	if len(events) != 1 || events[0].Kind != EventConnected || events[0].Address != tunnelAddr {
		t.Fatalf("Client events %+v", events)
	}

	if len(serverEvents) != 2 || serverEvents[0].Kind != ServerSecure || serverEvents[0].Code != SessionAuthenticated ||
		serverEvents[1].Kind != ServerOpened || serverEvents[1].User != 2 || serverEvents[1].Session != 1 {
		t.Fatalf("Server events %+v", serverEvents)
	}

	// A frame each way.
	if err := p.client.Send(p.now, &cemi.LDataReq{LData: *serverFrame(0)}); err != nil {
		t.Fatal(err)
	}

	p.pump()

	events, serverEvents = p.takeEvents()
	if len(events) != 1 || events[0].Kind != EventAcked || len(serverEvents) != 1 || serverEvents[0].Kind != ServerMessage {
		t.Fatalf("Events %+v, server events %+v", events, serverEvents)
	}

	ch := serverEvents[0].Channel
	p.server.Indicate(p.now, ch, &cemi.LDataInd{LData: *serverFrame(cemi.NewIndividualAddr3(1, 1, 1))})
	p.pump()

	if events, _ = p.takeEvents(); len(events) != 1 || events[0].Kind != EventMessage {
		t.Fatalf("Client events %+v", events)
	}

	// Heartbeats keep the session alive past its timeout.
	for i := 0; i < 5; i++ {
		p.advance(50 * time.Second)
	}

	if events, serverEvents = p.takeEvents(); len(events) != 0 || len(serverEvents) != 0 {
		t.Fatalf("Events %+v, server events %+v", events, serverEvents)
	}

	p.client.Disconnect(p.now)
	p.pump()

	_, serverEvents = p.takeEvents()
	if len(serverEvents) != 2 || serverEvents[0].Kind != ServerClosed || serverEvents[1].Code != SessionClose {
		t.Errorf("Server events %+v", serverEvents)
	}

	if plain != 0 {
		t.Errorf("%d plain packets", plain)
	}
}

func TestSecureTunnelFailures(t *testing.T) {
	tests := []struct {
		name               string
		user               uint8
		password, authCode string
		err                error
	}{
		{"wrong password", 2, "secret", "trustme", SessionAuthFailed},
		{"unknown user", 3, "tunnel", "trustme", SessionAuthFailed},
		{"wrong device authentication code", 2, "tunnel", "trustyou", ErrDeviceAuth},
	}

	for _, test := range tests {
		p := newSecurePair(t, test.user, test.password, test.authCode)

		events, _ := p.takeEvents()
		if len(events) != 1 || events[0].Kind != EventDisconnected || events[0].Err != test.err {
			t.Errorf("%s: client events %+v", test.name, events)
		}

		if len(p.server.Connections()) != 0 {
			t.Errorf("%s: connections %v", test.name, p.server.Connections())
		}
	}

	// Without a device authentication code, the client does not check the server.
	p := newSecurePair(t, 2, "tunnel", "")
	if events, _ := p.takeEvents(); len(events) != 1 || events[0].Kind != EventConnected {
		t.Errorf("Client events without a device authentication code %+v", events)
	}
}

func TestSecureServer(t *testing.T) {
	p := newSecurePair(t, 2, "tunnel", "trustme")
	p.takeEvents()

	// A plain tunnel is rejected.
	plain := &pair{t: t, server: p.server, client: NewTunnel(TunnelConfig{}), now: p.now}
	plain.client.Connect(plain.now)
	plain.pump()

	if events, _ := plain.takeEvents(); len(events) != 1 || events[0].Err != StatusConnectionType {
		t.Errorf("Plain tunnel events %+v", events)
	}

	// A management connection needs the management user.
	var sentPackets [][]byte
	p.drop = func(toClient bool, s Service) bool { return false }

	p.server.current = p.server.sessions[0]
	p.server.handle(p.now, clientEndpoint, &ConnectRequest{Type: DeviceManagementConnection})
	p.server.current = nil

	for {
		event, ok := p.server.NextEvent()
		if !ok {
			break
		}

		sentPackets = append(sentPackets, event.Packet)
	}

	if len(sentPackets) != 1 {
		t.Fatalf("Server sent %d packets", len(sentPackets))
	}

	s, _ := Decode(sentPackets[0])
	inner, err := s.(*SecureWrapper).Open(p.server.sessions[0].key)
	if err != nil {
		t.Fatal(err)
	}

	if r, _ := Decode(inner); r.(*ConnectResponse).Status != StatusConnectionType {
		t.Errorf("Management connection of user 2: %+v", r)
	}

	// Search responses tell that tunnelling needs security.
	d := p.server.Device()
	if !d.Supports(FamilySecurity) || len(d.Secured) != 2 {
		t.Errorf("Device %+v", d)
	}

	// A replayed packet is ignored.
	var last []byte
	p.drop = func(toClient bool, s Service) bool { return false }

	if err := p.client.Send(p.now, &cemi.LDataReq{LData: *serverFrame(0)}); err != nil {
		t.Fatal(err)
	}

	for {
		event, ok := p.client.NextEvent()
		if !ok {
			break
		}

		if event.Kind == EventSend {
			last = event.Packet
		}
	}

	p.server.HandlePacket(p.now, clientEndpoint, last)
	p.server.HandlePacket(p.now, clientEndpoint, last)

	messages := 0
	for {
		event, ok := p.server.NextEvent()
		if !ok {
			break
		}

		if event.Kind == ServerMessage {
			messages++
		}
	}

	if messages != 1 {
		t.Errorf("%d messages from a packet sent twice", messages)
	}

	// Without a valid packet, the session ends.
	p.now = p.now.Add(61 * time.Second)
	p.server.Tick(p.now)

	closed := 0
	for {
		event, ok := p.server.NextEvent()
		if !ok {
			break
		}

		if event.Kind == ServerClosed || event.Kind == ServerSecure && event.Code == SessionTimeout {
			closed++
		}
	}

	if closed != 2 || len(p.server.sessions) != 0 {
		t.Errorf("%d close events, %d sessions", closed, len(p.server.sessions))
	}
}

func TestSecurePackets(t *testing.T) {
	for _, s := range []Service{
		&SessionRequest{Control: clientEndpoint, PublicKey: *key32(an159Public)},
		&SessionResponse{Session: 1, PublicKey: *key32(an159Server), MAC: [16]byte{1, 2, 3}},
		&SessionResponse{},
		&SessionAuthenticate{User: 2, MAC: [16]byte{4, 5, 6}},
		&SessionStatus{Code: SessionKeepAlive},
		&SecureWrapper{Session: 7, Nonce: secure.Nonce{Seq: 0x123456789abc, Serial: [6]byte{1, 2, 3, 4, 5, 6}, Tag: 9}, Data: []byte{1, 2, 3}, MAC: [16]byte{7}},
	} {
		packet := Encode(s)

		got, err := Decode(packet)
		if err != nil {
			t.Errorf("Decode(% x): %v", packet, err)
			continue
		}

		if again := Encode(got); !bytes.Equal(again, packet) {
			t.Errorf("Encode(Decode(% x)) = % x", packet, again)
		}
	}

	if _, err := Decode(mustHex("06 10 09 50 00 10 00 01 00 00 00 00 00 00 00 00")); err == nil {
		t.Error("short SECURE_WRAPPER decoded")
	}
}
