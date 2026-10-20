package ets

// ETS allocates the tables and the parameters as relative segments: it asks the device for a
// segment of a given size (LdCtrlRelSegment), reads its address from PID_TABLE_REFERENCE and
// writes it with MemoryWrite. The segments therefore live in a 16-bit address space.
const (
	memoryStart = 0x1000
	memoryEnd   = 0x10000

	// MemorySize limits the total size of the segments, i.e. the RAM used by a download.
	MemorySize = 16 << 10
)

// A segment is a block of memory allocated for one table.
type segment struct {
	address uint16
	data    []byte
}

func (s *segment) end() int {
	return int(s.address) + align(len(s.data))
}

// align rounds a size up to 4 bytes, like the thelsing stack.
func align(n int) int {
	return (n + 3) &^ 3
}

// memory holds the allocated segments, sorted by address.
type memory struct {
	segments []*segment
}

func (m *memory) used() int {
	n := 0
	for _, s := range m.segments {
		n += align(len(s.data))
	}

	return n
}

// alloc allocates a zeroed segment at the lowest free address, or returns nil.
func (m *memory) alloc(size int) *segment {
	if size <= 0 || m.used()+align(size) > MemorySize {
		return nil
	}

	address := memoryStart
	i := 0

	for ; i < len(m.segments); i++ {
		if address+align(size) <= int(m.segments[i].address) {
			break
		}

		address = m.segments[i].end()
	}

	if address+align(size) > memoryEnd {
		return nil
	}

	s := &segment{address: uint16(address), data: make([]byte, size)}
	m.insert(i, s)

	return s
}

// add adds a segment restored from storage. It returns false if it does not fit.
func (m *memory) add(s *segment) bool {
	if int(s.address) < memoryStart || s.end() > memoryEnd || m.used()+align(len(s.data)) > MemorySize {
		return false
	}

	i := 0
	for i < len(m.segments) && m.segments[i].address < s.address {
		i++
	}

	if i > 0 && m.segments[i-1].end() > int(s.address) || i < len(m.segments) && s.end() > int(m.segments[i].address) {
		return false
	}

	m.insert(i, s)

	return true
}

func (m *memory) insert(i int, s *segment) {
	m.segments = append(m.segments, nil)
	copy(m.segments[i+1:], m.segments[i:])
	m.segments[i] = s
}

func (m *memory) free(s *segment) {
	for i, t := range m.segments {
		if t == s {
			m.segments = append(m.segments[:i], m.segments[i+1:]...)
			return
		}
	}
}

// find returns the n bytes at address, if they lie within one segment.
func (m *memory) find(address uint32, n int) []byte {
	for _, s := range m.segments {
		if address >= uint32(s.address) && address+uint32(n) <= uint32(s.address)+uint32(len(s.data)) {
			offset := int(address - uint32(s.address))
			return s.data[offset : offset+n]
		}
	}

	return nil
}
