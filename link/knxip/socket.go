//go:build !tinygo

package knxip

import "net"

// openSocket opens the socket of a client of the server at control. It is not connected, so that
// it can also reach a data endpoint of the server that differs from its control endpoint.
func openSocket(control *net.UDPAddr) (*net.UDPConn, error) {
	return net.ListenUDP("udp4", nil)
}
