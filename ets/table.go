package ets

// Load states, read from PID_LOAD_STATE_CONTROL.
const (
	stateUnloaded = 0
	stateLoaded   = 1
	stateLoading  = 2
	stateError    = 3
)

// Load events, written to PID_LOAD_STATE_CONTROL.
const (
	eventNoop                   = 0
	eventStartLoading           = 1
	eventLoadCompleted          = 2
	eventAdditionalLoadControls = 3
	eventUnload                 = 4
)

// The additional load control that allocates a relative segment (LdCtrlRelSegment).
const controlRelativeAllocation = 0x0B

// Error codes, read from PID_ERROR_CODE.
const (
	errNoFault          = 0
	errSoftwareFault    = 5
	errInvalidOpcode    = 11
	errMaxTableLength   = 13
	errUndefinedLoadCmd = 14
)

// A table is a load-controlled interface object: the address, association or group object table,
// or the application program, whose segment holds the parameters.
type table struct {
	objectType uint16
	state      uint8
	errorCode  uint8
	segment    *segment
}

// loadEvent runs the load state machine of KNX 03_05_02 as the thelsing stack implements it.
// data is the value written to PID_LOAD_STATE_CONTROL: the event, followed by the parameters of
// an additional load control.
func (t *table) loadEvent(m *memory, data []byte) {
	event := data[0]

	switch t.state {
	case stateUnloaded:
		switch event {
		case eventNoop, eventLoadCompleted, eventAdditionalLoadControls, eventUnload:
		case eventStartLoading:
			t.state = stateLoading
		default:
			t.fail(errUndefinedLoadCmd)
		}

	case stateLoading:
		switch event {
		case eventNoop, eventStartLoading:
		case eventLoadCompleted:
			// Like the thelsing stack, a group object table must have been allocated.
			if t.objectType == objectTypeGroupObjects && t.segment == nil {
				t.fail(errSoftwareFault)
			} else {
				t.state = stateLoaded
			}
		case eventUnload:
			t.unload(m)
		case eventAdditionalLoadControls:
			t.additionalLoadControls(m, data)
		default:
			t.fail(errUndefinedLoadCmd)
		}

	case stateLoaded:
		switch event {
		case eventNoop, eventLoadCompleted:
		case eventStartLoading:
			t.state = stateLoading
		case eventUnload:
			t.unload(m)
		case eventAdditionalLoadControls:
			t.fail(errInvalidOpcode)
		default:
			t.fail(errUndefinedLoadCmd)
		}

	case stateError:
		switch event {
		case eventUnload:
			t.unload(m)
		case eventNoop, eventLoadCompleted, eventAdditionalLoadControls, eventStartLoading:
		default:
			t.fail(errUndefinedLoadCmd)
		}
	}
}

func (t *table) fail(code uint8) {
	t.state = stateError
	t.errorCode = code
}

// unload frees the segment. Unlike the thelsing stack, which frees it only when unloading a
// loaded table, it is freed in every state, so an aborted download leaks nothing.
func (t *table) unload(m *memory) {
	t.state = stateUnloaded
	t.errorCode = errNoFault

	if t.segment != nil {
		m.free(t.segment)
		t.segment = nil
	}
}

// additionalLoadControls handles a load control that ETS sends while loading. The only one a
// System B device supports is the allocation of a relative segment:
//
//	03 0B ss ss ss ss mm ff …: size (32 bits), mode (1: fill), fill byte
func (t *table) additionalLoadControls(m *memory, data []byte) {
	if len(data) < 8 || data[1] != controlRelativeAllocation {
		t.fail(errInvalidOpcode)
		return
	}

	size := uint32(data[2])<<24 | uint32(data[3])<<16 | uint32(data[4])<<8 | uint32(data[5])

	if t.segment != nil {
		m.free(t.segment)
		t.segment = nil
	}

	if size == 0 {
		return
	}

	if size > memoryEnd {
		t.fail(errMaxTableLength)
		return
	}

	t.segment = m.alloc(int(size))
	if t.segment == nil {
		t.fail(errMaxTableLength)
		return
	}

	if data[6] == 1 {
		for i := range t.segment.data {
			t.segment.data[i] = data[7]
		}
	}
}

// reference returns PID_TABLE_REFERENCE: the address of the segment, or 0.
func (t *table) reference() uint32 {
	if t.state == stateUnloaded || t.segment == nil {
		return 0
	}

	return uint32(t.segment.address)
}

// mcb returns PID_MCB_TABLE, the memory control block of a loaded table: the segment size, CRC
// control (0: always valid), access levels and the CRC of the data.
func (t *table) mcb() []byte {
	if t.state != stateLoaded {
		return nil
	}

	var data []byte
	if t.segment != nil {
		data = t.segment.data
	}

	size := len(data)
	crc := crc16(data)

	return []byte{byte(size >> 24), byte(size >> 16), byte(size >> 8), byte(size), 0x00, 0xFF, byte(crc >> 8), byte(crc)}
}

// data returns the content of a loaded table.
func (t *table) data() []byte {
	if t.state != stateLoaded || t.segment == nil {
		return nil
	}

	return t.segment.data
}

// crc16 computes the CRC-16 CCITT of the thelsing stack: polynomial 0x1021, initial value 0xFFFF,
// over the data followed by two zero bytes.
func crc16(data []byte) uint16 {
	crc := uint32(0xFFFF)

	for i := 0; i < 8*(len(data)+2); i++ {
		crc <<= 1

		if i/8 < len(data) {
			crc |= uint32(data[i/8]>>(7-i%8)) & 1
		}

		if crc&0x10000 != 0 {
			crc ^= 0x1021
		}
	}

	return uint16(crc)
}
