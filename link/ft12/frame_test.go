package ft12

import (
	"bytes"
	"testing"
)

func TestAppendVariable(t *testing.T) {
	// T_Connect.req to 1.1.3, as sent to a kBerry.
	payload := []byte{0x11, 0x00, 0xbc, 0x60, 0x00, 0x00, 0x11, 0x03, 0x00, 0x80}
	expected := append([]byte{0x68, 0x0b, 0x0b, 0x68, 0x73}, payload...)
	expected = append(expected, (0x73+0x11+0xbc+0x60+0x11+0x03+0x80)%256, 0x16)

	if frame := AppendVariable(nil, 0x73, payload); !bytes.Equal(frame, expected) {
		t.Errorf("Frame % x, expected % x", frame, expected)
	}
}

func TestDecoder(t *testing.T) {
	payload := []byte{0x29, 0x00, 0xbc, 0xe0, 0x11, 0x01, 0x0a, 0x03, 0x01, 0x00, 0x81}

	var stream []byte
	stream = append(stream, 0x00, 0x68, 0x42) // garbage
	stream = append(stream, Ack)              // ack
	stream = append(stream, AppendFixed(nil, ControlResetInd)...)
	bad := AppendVariable(nil, 0xf3, payload)
	bad[len(bad)-2]++ // broken checksum
	stream = append(stream, bad...)
	stream = append(stream, AppendVariable(nil, 0xd3, payload)...)
	stream = append(stream, Ack)

	expected := []Frame{
		{Kind: KindAck},
		{Kind: KindFixed, Control: ControlResetInd},
		{Kind: KindVariable, Control: 0xd3, Payload: payload},
		{Kind: KindAck},
	}

	// Feed the stream in every possible chunk size; the result must not depend on it.
	for chunk := 1; chunk <= len(stream); chunk++ {
		var decoder Decoder
		var frames []Frame

		for i := 0; i < len(stream); i += chunk {
			end := i + chunk
			if end > len(stream) {
				end = len(stream)
			}

			decoder.Write(stream[i:end])

			for {
				frame, ok := decoder.Next()
				if !ok {
					break
				}

				frames = append(frames, frame)
			}
		}

		if len(frames) != len(expected) {
			t.Fatalf("Chunk size %d: got %d frames %+v, expected %d", chunk, len(frames), frames, len(expected))
		}

		for i, frame := range frames {
			want := expected[i]
			if frame.Kind != want.Kind || frame.Control != want.Control ||
				!bytes.Equal(frame.Payload, want.Payload) {
				t.Errorf("Chunk size %d, frame %d: got %+v, expected %+v", chunk, i, frame, want)
			}
		}
	}
}
