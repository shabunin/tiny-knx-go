package ets

// The image saved in Storage:
//
//	'E' 1                  magic, version
//	[5]                    PID_PROG_VERSION
//	4 × state, error code, segment address (2), size (2), data
//
// A table without a segment has address and size 0.
const imageVersion = 1

// encode returns the image of the tables. e.mu is held.
func (e *Device) encode() []byte {
	image := append([]byte{'E', imageVersion}, e.progVersion[:]...)

	for i := range e.tables {
		t := &e.tables[i]
		image = append(image, t.state, t.errorCode)

		if s := t.segment; s != nil {
			image = append(image, byte(s.address>>8), byte(s.address), byte(len(s.data)>>8), byte(len(s.data)))
			image = append(image, s.data...)
		} else {
			image = append(image, 0, 0, 0, 0)
		}
	}

	return image
}

// decode restores the tables from an image. e.mu is held or not needed.
func (e *Device) decode(image []byte) bool {
	if len(image) < 7 || image[0] != 'E' || image[1] != imageVersion {
		return false
	}

	copy(e.progVersion[:], image[2:7])
	image = image[7:]

	for i := range e.tables {
		if len(image) < 6 {
			return false
		}

		t := &e.tables[i]
		t.state, t.errorCode = image[0], image[1]
		address := uint16(image[2])<<8 | uint16(image[3])
		size := int(image[4])<<8 | int(image[5])
		image = image[6:]

		if t.state > stateError || len(image) < size {
			return false
		}

		if size > 0 {
			t.segment = &segment{address: address, data: append([]byte(nil), image[:size]...)}
			if !e.memory.add(t.segment) {
				return false
			}
		}

		image = image[size:]
	}

	return len(image) == 0
}
