//go:build !unix && !tinygo

package knxip

import "net"

func listenReusable(addr *net.UDPAddr) (*net.UDPConn, error) {
	return net.ListenUDP("udp4", addr)
}
