package knxip

import (
	"net"
	"time"

	"github.com/shabunin/tiny-knx-go/knxnet"
)

// A Server is a KNXnet/IP interface or router that answered a search.
type Server struct {
	// Control is the address for tunnelling connections (DialTunnel).
	Control *net.UDPAddr
	Info    knxnet.DeviceInfo
}

// Search looks for KNXnet/IP servers: it sends a SEARCH_REQUEST to the routing group from the
// given interface (nil for the one the system routes multicast through) and collects the
// responses that arrive within timeout.
func Search(ifi *net.Interface, timeout time.Duration) ([]Server, error) {
	group := &net.UDPAddr{IP: net.IP(knxnet.RoutingGroup[:]), Port: knxnet.Port}

	var local net.IP

	if ifi != nil {
		local = interfaceIPv4(ifi)
	} else if probe, err := net.DialUDP("udp4", nil, group); err == nil {
		// Connecting a UDP socket selects the source address without sending anything.
		local = probe.LocalAddr().(*net.UDPAddr).IP
		probe.Close()
	}

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: local})
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	return search(conn, group, timeout)
}

// search sends a search request to dest and collects the responses on conn.
func search(conn *net.UDPConn, dest *net.UDPAddr, timeout time.Duration) ([]Server, error) {
	self := conn.LocalAddr().(*net.UDPAddr)

	request := &knxnet.SearchRequest{Discovery: knxnet.HPAI{Protocol: knxnet.ProtocolUDP, Port: uint16(self.Port)}}
	copy(request.Discovery.IP[:], self.IP.To4())

	if _, err := conn.WriteToUDP(knxnet.Encode(request), dest); err != nil {
		return nil, err
	}

	conn.SetReadDeadline(time.Now().Add(timeout))

	var servers []Server

	seen := make(map[string]bool)
	buffer := make([]byte, 512)

	for {
		n, from, err := conn.ReadFromUDP(buffer)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				return servers, nil
			}

			return servers, err
		}

		s, err := knxnet.Decode(buffer[:n])
		if err != nil {
			continue
		}

		response, ok := s.(*knxnet.SearchResponse)
		if !ok {
			continue
		}

		control := &net.UDPAddr{IP: net.IP(response.Control.IP[:]), Port: int(response.Control.Port)}
		if control.IP.IsUnspecified() || control.Port == 0 {
			control = from
		}

		if key := control.String(); !seen[key] {
			seen[key] = true
			servers = append(servers, Server{Control: control, Info: response.Device})
		}
	}
}
