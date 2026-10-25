package gateway

import (
	"fmt"
	"log"
	"net"

	"github.com/shabunin/tiny-knx-go/cemi"
	"github.com/shabunin/tiny-knx-go/internal/pretty"
	"github.com/shabunin/tiny-knx-go/knxnet"
	"github.com/shabunin/tiny-knx-go/link"
	"github.com/shabunin/tiny-knx-go/secure"
)

// PrintTelegrams prints the frames on the bus.
func PrintTelegrams(l link.Link) {
	for ind := range l.Inbound() {
		log.Print(pretty.Frame(&ind.LData))
	}
}

// PrintPacket prints a KNXnet/IP packet that the server received or sent.
func PrintPacket(received bool, peer knxnet.HPAI, packet []byte) {
	direction := "->"
	if received {
		direction = "<-"
	}

	endpoint := func(h knxnet.HPAI) string { return fmt.Sprintf("%v:%d", net.IP(h.IP[:]), h.Port) }

	var text string

	service, err := knxnet.Decode(packet)
	switch s := service.(type) {
	case nil:
		text = fmt.Sprintf("% x (%v)", packet, err)
	case *knxnet.ConnectRequest:
		text = fmt.Sprintf("ConnectRequest type %d, control %s, data %s", s.Type, endpoint(s.Control), endpoint(s.Data))
	case *knxnet.ConnectResponse:
		text = fmt.Sprintf("ConnectResponse channel %d, %v, data %s, address %v", s.Channel, s.Status, endpoint(s.Data), s.Address)
	case *knxnet.ConnectionStateRequest:
		text = fmt.Sprintf("ConnectionStateRequest channel %d, control %s", s.Channel, endpoint(s.Control))
	case *knxnet.ConnectionStateResponse:
		text = fmt.Sprintf("ConnectionStateResponse channel %d, %v", s.Channel, s.Status)
	case *knxnet.DisconnectRequest:
		text = fmt.Sprintf("DisconnectRequest channel %d, control %s", s.Channel, endpoint(s.Control))
	case *knxnet.DisconnectResponse:
		text = fmt.Sprintf("DisconnectResponse channel %d, %v", s.Channel, s.Status)
	case *knxnet.TunnellingRequest:
		text = fmt.Sprintf("TunnellingRequest channel %d seq %d: %s", s.Channel, s.Sequence, Message(s.Message))
	case *knxnet.TunnellingAck:
		text = fmt.Sprintf("TunnellingAck channel %d seq %d, %v", s.Channel, s.Sequence, s.Status)
	case *knxnet.DeviceConfigRequest:
		text = fmt.Sprintf("DeviceConfigRequest channel %d seq %d: %s", s.Channel, s.Sequence, Message(s.Message))
	case *knxnet.DeviceConfigAck:
		text = fmt.Sprintf("DeviceConfigAck channel %d seq %d, %v", s.Channel, s.Sequence, s.Status)
	default:
		text = fmt.Sprintf("%T", service)
	}

	log.Printf("%s %s %s", direction, endpoint(peer), text)
}

// Message describes a cEMI message.
func Message(m cemi.Message) string {
	switch m := m.(type) {
	case *cemi.LDataReq:
		return "L_Data.req " + pretty.Frame(&m.LData)
	case *cemi.LDataInd:
		return "L_Data.ind " + pretty.Frame(&m.LData)
	case *cemi.LDataCon:
		if m.Control1&cemi.Control1HasError != 0 {
			return "L_Data.con (negative) " + pretty.Frame(&m.LData)
		}

		return "L_Data.con " + pretty.Frame(&m.LData)
	}

	return fmt.Sprintf("%T %+v", m, m)
}

// SecureConfig gives the management user and a tunnelling user per tunnel the same password, as
// ETS does not tell which user it will authenticate as before it tries.
func SecureConfig(password, authCode string, tunnels int) *knxnet.SecureConfig {
	key := secure.UserPasswordKey(password)
	users := map[uint8][16]byte{}

	for user := 1; user <= 1+tunnels; user++ {
		users[uint8(user)] = key
	}

	return &knxnet.SecureConfig{DeviceAuth: secure.DeviceAuthKey(authCode), Users: users}
}

// InterfaceSerial returns the serial number of a KNXnet/IP interface of the manufacturer. It
// differs by port, because ETS tells interfaces apart by their serial numbers.
func InterfaceSerial(manufacturer uint16, port int) [6]byte {
	if port == knxnet.Port {
		port = 1
	}

	return [6]byte{byte(manufacturer >> 8), byte(manufacturer), 0xff, 0xff, byte(port >> 8), byte(port)}
}
