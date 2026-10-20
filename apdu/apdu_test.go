package apdu

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/shabunin/tiny-knx-go/cemi"
	"github.com/shabunin/tiny-knx-go/dpt"
)

var allServices = []APDU{
	GroupValueRead{},
	GroupValueResponse{Data: []byte{1}},
	GroupValueWrite{Data: []byte{0, 0x0c, 0x1a}},
	IndividualAddressWrite{Address: cemi.NewIndividualAddr3(1, 1, 50)},
	IndividualAddressRead{},
	IndividualAddressResponse{},
	IndividualAddressSerialNumberRead{SerialNumber: SerialNumber{0x00, 0xfa, 1, 2, 3, 4}},
	IndividualAddressSerialNumberResponse{SerialNumber: SerialNumber{0x00, 0xfa, 1, 2, 3, 4}, DomainAddress: 0},
	IndividualAddressSerialNumberWrite{
		SerialNumber: SerialNumber{0x00, 0xfa, 1, 2, 3, 4},
		Address:      cemi.NewIndividualAddr3(1, 1, 50),
	},
	ADCRead{Channel: 1, Count: 8},
	ADCResponse{Channel: 1, Count: 8, Value: 0x1234},
	MemoryRead{Count: 12, Address: 0x0060},
	MemoryResponse{Address: 0x0060, Data: []byte{1, 2, 3}},
	MemoryWrite{Address: 0x0060, Data: []byte{4, 5}},
	UserManufacturerInfoRead{},
	UserManufacturerInfoResponse{Manufacturer: 0xfa, Data: [2]byte{1, 2}},
	DeviceDescriptorRead{Type: 2},
	DeviceDescriptorResponse{Type: 0, Descriptor: []byte{0x07, 0xb0}},
	Restart{},
	Restart{MasterReset: true, EraseCode: 1, Channel: 0},
	RestartResponse{ErrorCode: 0, ProcessTime: 5},
	AuthorizeRequest{Key: 0xffffffff},
	AuthorizeResponse{Level: 0},
	KeyWrite{Level: 3, Key: 0x12345678},
	KeyResponse{Level: 3},
	PropertyValueRead{PropertyRef{Object: 0, PropertyID: 11, Count: 1, Start: 1}},
	PropertyValueResponse{PropertyRef{Object: 0, PropertyID: 11, Count: 1, Start: 1}, []byte{0, 0xfa, 1, 2, 3, 4}},
	PropertyValueWrite{PropertyRef{Object: 4, PropertyID: 13, Count: 1, Start: 1}, []byte{1, 2, 3, 4, 5}},
	PropertyDescriptionRead{Object: 0, PropertyID: 11},
	PropertyDescriptionResponse{
		Object: 0, PropertyID: 11, WriteEnable: true, Type: 0x11, MaxElements: 1, ReadLevel: 3, WriteLevel: 0,
	},
}

func pack(app *cemi.AppData) []byte {
	buffer := make([]byte, app.Size())
	app.Pack(buffer)

	return buffer
}

func TestRoundTrip(t *testing.T) {
	for _, original := range allServices {
		t.Run(fmt.Sprintf("%T", original), func(t *testing.T) {
			encoded := Encode(original)

			// Go through the wire format, as a received frame would.
			var unit cemi.LData
			frame := (&cemi.LDataInd{LData: cemi.LData{Data: encoded}})
			buffer := make([]byte, cemi.Size(frame))
			cemi.Pack(buffer, frame)

			var msg cemi.Message
			if _, err := cemi.Unpack(buffer, &msg); err != nil {
				t.Fatalf("Unpack failed: %v", err)
			}

			unit = msg.(*cemi.LDataInd).LData

			decoded, err := Decode(unit.Data.(*cemi.AppData))
			if err != nil {
				t.Fatalf("Decode failed: %v", err)
			}

			if got, want := fmt.Sprintf("%T %+v", decoded, decoded), fmt.Sprintf("%T %+v", original, original); got != want {
				t.Errorf("Decoded %s, expected %s", got, want)
			}

			if !bytes.Equal(pack(Encode(decoded)), pack(encoded)) {
				t.Errorf("Re-encoded % x, expected % x", pack(Encode(decoded)), pack(encoded))
			}
		})
	}
}

func TestWireFormat(t *testing.T) {
	testCases := []struct {
		name string
		apdu APDU
		tpdu string // length byte, TPCI/APCI, APCI/data, data…
	}{
		// From the kBerry trace in bb_digging_knx (sent numbered; here unnumbered).
		{"DeviceDescriptorRead", DeviceDescriptorRead{}, "010300"},
		{"DeviceDescriptorResponse", DeviceDescriptorResponse{Descriptor: []byte{0x07, 0x01}}, "0303400701"},

		// ETS reading the serial number: object 0, PID 11, 1 element from index 1.
		{"PropertyValueRead", PropertyValueRead{PropertyRef{0, 11, 1, 1}}, "0503d5000b1001"},

		{"GroupValueWrite DPT 1", GroupValueWrite{Data: dpt.DPT_1001(true).Pack()}, "010081"},
		{"GroupValueWrite DPT 9", GroupValueWrite{Data: dpt.DPT_9001(20.5).Pack()}, "0300800c01"},
		{"GroupValueRead", GroupValueRead{}, "010000"},
		{"Restart", Restart{}, "010380"},
		{"MemoryRead", MemoryRead{Count: 4, Address: 0x0104}, "0302040104"},
		{"IndividualAddressWrite", IndividualAddressWrite{Address: cemi.NewIndividualAddr3(1, 1, 50)}, "0300c01132"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			expected, _ := hex.DecodeString(tc.tpdu)

			if got := pack(Encode(tc.apdu)); !bytes.Equal(got, expected) {
				t.Errorf("Encoded % x, expected % x", got, expected)
			}
		})
	}
}

func TestADCResponseVersusExtendedServices(t *testing.T) {
	adc, err := Decode(&cemi.AppData{Command: cemi.AdcResponse, Data: []byte{0x03, 8, 0x12, 0x34}})
	if err != nil {
		t.Fatal(err)
	}

	if got, ok := adc.(ADCResponse); !ok || got.Channel != 3 || got.Value != 0x1234 {
		t.Errorf("Expected ADCResponse on channel 3, got %T %+v", adc, adc)
	}

	ext, err := Decode(&cemi.AppData{Command: cemi.AdcResponse, Data: []byte{0x08, 0, 0}})
	if err != nil {
		t.Fatal(err)
	}

	if got, ok := ext.(Unknown); !ok || got.Service != ServiceSystemNetworkParameterRead {
		t.Errorf("Expected SystemNetworkParameterRead, got %T %+v", ext, ext)
	}
}

func TestTooShort(t *testing.T) {
	for _, original := range allServices {
		raw := original.Raw()
		if len(raw.Data) == 0 {
			continue
		}

		// The last 4 bytes are reserved and may be omitted.
		if _, ok := original.(IndividualAddressSerialNumberWrite); ok {
			continue
		}

		raw.Data = raw.Data[:len(raw.Data)-1]

		// Services with variable-length data accept shorter data; the others must fail.
		decoded, err := DecodeRaw(raw)
		if err == nil && Encode(decoded).Size() >= Encode(original).Size() {
			t.Errorf("%T: truncated APDU decoded as %+v", original, decoded)
		}
	}
}

func TestUnknownService(t *testing.T) {
	raw := Raw{Service: ServiceFunctionPropertyCommand, Data: []byte{1, 2, 3}}

	decoded, err := DecodeRaw(raw)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(pack(Encode(decoded)), pack(raw.AppData())) {
		t.Errorf("Unknown service did not survive a round trip: %+v", decoded)
	}
}
