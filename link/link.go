// Package link defines the interface between the KNX protocol stack and a bus connection (the data
// link layer). Implementations for different media live in sub-packages.
package link

import (
	"errors"

	"github.com/shabunin/tiny-knx-go/cemi"
)

var (
	// ErrNotConfirmed is returned by Send when the frame was transmitted but the bus did not
	// confirm it, e.g. because no device acknowledged it.
	ErrNotConfirmed = errors.New("link: frame not confirmed by the bus")

	// ErrConfirmTimeout is returned by Send when no confirmation arrived in time.
	ErrConfirmTimeout = errors.New("link: confirmation timeout")

	// ErrClosed is returned when the link has been closed.
	ErrClosed = errors.New("link: closed")
)

// A Link is a connection to a KNX bus at the data link layer.
//
// Every medium confirms frames differently: a BAOS module sends L_Data.con, a TP-UART reports the
// bus acknowledgement and an IP router confirms nothing. Implementations hide this, so that Send
// behaves the same everywhere.
type Link interface {
	// Send transmits a frame and returns once the bus has confirmed it.
	Send(req *cemi.LDataReq) error

	// Inbound returns the channel that receives frames from the bus. It is closed when the link
	// closes.
	Inbound() <-chan *cemi.LDataInd

	// Close terminates the link.
	Close() error
}
