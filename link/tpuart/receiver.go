package tpuart

import "time"

// rxKind tells the Link what a received byte completed.
type rxKind uint8

const (
	rxNothing      rxKind = iota
	rxFrameStart          // the first byte of a frame
	rxHeader              // the addresses of the frame are known: time to acknowledge it
	rxFrame               // a complete frame with a valid checksum is in receiver.frame
	rxInvalidFrame        // a frame was broken off or had a wrong checksum
	rxReset               // U_Reset.ind
	rxStateInd            // U_State.ind, in receiver.value
	rxConfirm             // L_Data.con; receiver.value is non-zero if the frame was acknowledged
	rxUnknown             // a byte that is not a known service; the following bytes are ignored
)

type rxState uint8

const (
	rxIdle    rxState = iota // waiting for a service or the start of a frame
	rxInFrame                // collecting frame bytes
	rxInvalid                // discarding bytes until the line is quiet
)

// A receiver splits the byte stream from the chip into services and frames. It does no I/O.
//
// The stream has no framing: a frame ends when its length field says so, and after an error the
// receiver cannot tell frame bytes from services. It then waits for a pause on the line, as the
// bus has at least 50 bit times (about 5 ms) between frames.
type receiver struct {
	frameTimeout time.Duration // pause that breaks off a frame
	resyncGap    time.Duration // pause that ends the invalid state

	state rxState
	frame []byte
	value byte
	last  time.Time
}

func newReceiver(frameTimeout, resyncGap time.Duration) *receiver {
	return &receiver{
		frameTimeout: frameTimeout,
		resyncGap:    resyncGap,
		frame:        make([]byte, 0, maxFrameLength),
	}
}

// feed processes the next byte, received at now.
func (r *receiver) feed(b byte, now time.Time) (kind rxKind) {
	gap := now.Sub(r.last)
	r.last = now

	switch {
	case r.state == rxInFrame && gap > r.frameTimeout:
		// The rest of the frame never came; b starts something new.
		r.state = rxIdle

		if kind = r.idle(b); kind == rxNothing {
			kind = rxInvalidFrame
		}

		return kind

	case r.state == rxInvalid && gap > r.resyncGap:
		r.state = rxIdle
	}

	switch r.state {
	case rxInFrame:
		return r.frameByte(b)

	case rxIdle:
		return r.idle(b)
	}

	return rxNothing
}

func (r *receiver) frameByte(b byte) rxKind {
	r.frame = append(r.frame, b)

	if len(r.frame) == 6 {
		return rxHeader
	}

	size, ok := frameSize(r.frame)
	if !ok || len(r.frame) < size {
		return rxNothing
	}

	if Checksum(r.frame[:size-1]) != r.frame[size-1] {
		r.state = rxInvalid
		return rxInvalidFrame
	}

	r.state = rxIdle

	return rxFrame
}

// idle classifies a byte outside a frame, in the order the reference implementation uses.
func (r *receiver) idle(b byte) rxKind {
	switch {
	case isFrameStart(b):
		r.frame = append(r.frame[:0], b)
		r.state = rxInFrame

		return rxFrameStart

	case b == ResetInd:
		return rxReset

	case b&stateIndMask == StateInd:
		r.value = b

		return rxStateInd

	case b&configIndMask == configInd, b == stopModeInd, b&ackIndMask == ackInd,
		b == pollDataInd, b&frameStateMask == frameStateInd, b == systemStatInd:
		return rxNothing

	case b&confirmMask == ConfirmInd:
		r.value = b & confirmSuccess

		return rxConfirm
	}

	r.state = rxInvalid

	return rxUnknown
}
