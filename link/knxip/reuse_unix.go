//go:build unix

package knxip

import (
	"context"
	"net"
	"syscall"
)

// listenReusable opens a UDP socket with SO_REUSEADDR, so that it can share its port with the
// multicast sockets of routing participants.
func listenReusable(addr *net.UDPAddr) (*net.UDPConn, error) {
	config := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var err error

		c.Control(func(fd uintptr) {
			err = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
		})

		return err
	}}

	conn, err := config.ListenPacket(context.Background(), "udp4", addr.String())
	if err != nil {
		return nil, err
	}

	return conn.(*net.UDPConn), nil
}
