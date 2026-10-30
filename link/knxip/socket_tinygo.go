//go:build tinygo

package knxip

import "net"

// openSocket opens the socket of a client of the server at control. TinyGo's UDP sockets
// (netdev) are always connected: they send only to control and report it as the source of every
// packet, so the server must take data at its control endpoint, as most servers do.
func openSocket(control *net.UDPAddr) (*net.UDPConn, error) {
	return net.DialUDP("udp4", nil, control)
}
