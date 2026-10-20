//go:build linux && !tinygo

// Package serial opens serial ports on Linux using only the standard library, e.g. the UART of a
// Raspberry Pi with a kBerry module (/dev/serial0) or a USB adapter (/dev/ttyUSB0).
package serial

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// Parity is the parity mode of a serial port.
type Parity uint8

// These are the supported parity modes.
const (
	ParityNone Parity = iota
	ParityEven
	ParityOdd
)

var baudRates = map[int]uint32{
	9600:   syscall.B9600,
	19200:  syscall.B19200,
	38400:  syscall.B38400,
	57600:  syscall.B57600,
	115200: syscall.B115200,
}

// Open opens a serial port in raw mode with 8 data bits and 1 stop bit. Closing the returned file
// interrupts pending reads.
func Open(path string, baudRate int, parity Parity) (*os.File, error) {
	speed, ok := baudRates[baudRate]
	if !ok {
		return nil, fmt.Errorf("serial: unsupported baud rate %d", baudRate)
	}

	// O_NONBLOCK makes the os package use the runtime poller, so that Close unblocks Read.
	file, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOCTTY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}

	termios := syscall.Termios{
		Cflag:  syscall.CS8 | syscall.CREAD | syscall.CLOCAL | speed,
		Ispeed: speed,
		Ospeed: speed,
	}

	switch parity {
	case ParityEven:
		termios.Cflag |= syscall.PARENB
	case ParityOdd:
		termios.Cflag |= syscall.PARENB | syscall.PARODD
	}

	// Return from read as soon as one byte is available.
	termios.Cc[syscall.VMIN] = 1
	termios.Cc[syscall.VTIME] = 0

	conn, err := file.SyscallConn()
	if err != nil {
		file.Close()
		return nil, err
	}

	var errno syscall.Errno

	err = conn.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall(
			syscall.SYS_IOCTL, fd, syscall.TCSETS, uintptr(unsafe.Pointer(&termios)),
		)
	})
	if err == nil && errno != 0 {
		err = errno
	}

	if err != nil {
		file.Close()
		return nil, fmt.Errorf("serial: configuring %s: %w", path, err)
	}

	return file, nil
}
