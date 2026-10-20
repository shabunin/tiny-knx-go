package product

import (
	"bytes"
	"encoding/xml"
	"io"
	"strings"
	"testing"

	"github.com/shabunin/tiny-knx-go/device"
)

type params []byte

func (p params) ParamBytes(offset, n int) []byte {
	if offset+n > len(p) {
		return nil
	}

	return p[offset : offset+n]
}

func testProduct() *Product {
	return &Product{
		Name:        "Test <device>",
		OrderNumber: "TEST1",
		Application: 0x7a01,
		Version:     0x12,
		Blocks: []Block{
			{
				Name: "General",
				Params: []Param{
					{Name: "mode", Text: "Mode", Offset: 0, Bits: 8, Default: 1, Values: []Value{{0, "Off"}, {1, "On"}}},
					{Name: "time", Text: "Time & delay", Offset: 2, Bits: 16, Default: 300, Min: 1, Max: 3600},
				},
				Objects: []Object{
					{Name: "Switch", Function: "On/Off", DPT: "1.001", Flags: device.FlagCommunicate | device.FlagWrite},
				},
			},
			{
				Name: "Temperature",
				Objects: []Object{
					{Name: "Temperature", Function: "Value", DPT: "9.001", Flags: device.FlagCommunicate | device.FlagRead | device.FlagTransmit},
				},
			},
		},
	}
}

func TestValue(t *testing.T) {
	p := testProduct()

	if got := p.ParamSize(); got != 4 {
		t.Errorf("ParamSize = %d, want 4", got)
	}

	loaded := params{0, 0, 0x0e, 0x10}
	if got := p.Value(loaded, "time"); got != 3600 {
		t.Errorf("time = %d, want 3600", got)
	}

	if got := p.Value(loaded, "mode"); got != 0 {
		t.Errorf("mode = %d, want 0", got)
	}

	if got := p.Value(params(nil), "time"); got != 300 {
		t.Errorf("time without parameters = %d, want the default 300", got)
	}

	defer func() {
		if recover() == nil {
			t.Error("Value of an unknown parameter did not panic")
		}
	}()

	p.Value(loaded, "nothing")
}

func TestObjects(t *testing.T) {
	p := testProduct()

	objects := p.Objects()
	if len(objects) != 2 || objects[0].Name != "Switch" || objects[1].DPT != "9.001" {
		t.Errorf("Objects = %+v", objects)
	}

	if got, want := p.ProgramVersion(), [5]byte{0x00, 0xfa, 0x7a, 0x01, 0x12}; got != want {
		t.Errorf("ProgramVersion = % x, want % x", got, want)
	}
}

func TestCheck(t *testing.T) {
	if err := testProduct().Check(); err != nil {
		t.Fatalf("Check: %v", err)
	}

	tests := []struct {
		name   string
		change func(p *Product)
		want   string
	}{
		{"order number", func(p *Product) { p.OrderNumber = "TEST-1" }, "letters and digits"},
		{"overlap", func(p *Product) { p.Blocks[0].Params[1].Offset = 0 }, "overlap"},
		{"bits", func(p *Product) { p.Blocks[0].Params[0].Bits = 4 }, "4 bits"},
		{"default", func(p *Product) { p.Blocks[0].Params[1].Default = 0 }, "default 0"},
		{"enumeration default", func(p *Product) { p.Blocks[0].Params[0].Default = 2 }, "default 2"},
		{"duplicate", func(p *Product) { p.Blocks[1].Objects[0].Name = "Switch" }, "duplicate"},
		{"DPT", func(p *Product) { p.Blocks[1].Objects[0].DPT = "16.001" }, "unsupported DPT"},
	}

	for _, test := range tests {
		p := testProduct()
		test.change(p)

		if err := p.Check(); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: Check = %v, want %q", test.name, err, test.want)
		}
	}
}

func TestWriteXML(t *testing.T) {
	var b bytes.Buffer
	if err := testProduct().WriteXML(&b); err != nil {
		t.Fatalf("WriteXML: %v", err)
	}

	// The document is well-formed and has the elements and attributes ETS reads.
	attrs := map[string]map[string]string{}
	count := map[string]int{}

	decoder := xml.NewDecoder(bytes.NewReader(b.Bytes()))
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("Invalid XML: %v\n%s", err, b.String())
		}

		if start, ok := token.(xml.StartElement); ok {
			count[start.Name.Local]++

			// The first element with attributes, e.g. not the list <Hardware> around <Hardware Id=...>.
			if attrs[start.Name.Local] == nil && len(start.Attr) > 0 {
				attrs[start.Name.Local] = map[string]string{}
				for _, a := range start.Attr {
					attrs[start.Name.Local][a.Name.Local] = a.Value
				}
			}
		}
	}

	checks := []struct{ element, attr, want string }{
		{"ApplicationProgram", "Id", "M-00FA_A-7A01-12-0000"},
		{"ApplicationProgram", "ApplicationNumber", "31233"},
		{"ApplicationProgram", "ApplicationVersion", "18"},
		{"ApplicationProgram", "MaskVersion", "MV-07B0"},
		{"ApplicationProgram", "Name", "Test <device>"},
		{"RelativeSegment", "Size", "4"},
		{"Hardware", "Id", "M-00FA_H-7A01-1"},
		{"Product", "Id", "M-00FA_H-7A01-1_P-TEST1"},
		{"Product", "OrderNumber", "TEST1"},
		{"ComObject", "DatapointType", "DPST-1-1"},
		{"ComObject", "ObjectSize", "1 Bit"},
		{"ComObject", "Number", "1"},
		{"ComObject", "WriteFlag", "Enabled"},
		{"ComObject", "ReadFlag", "Disabled"},
		{"Parameter", "Value", "1"},
	}

	for _, c := range checks {
		if got := attrs[c.element][c.attr]; got != c.want {
			t.Errorf("%s %s = %q, want %q", c.element, c.attr, got, c.want)
		}
	}

	for element, want := range map[string]int{"ParameterBlock": 2, "Parameter": 2, "ComObject": 2, "Enumeration": 2} {
		if count[element] != want {
			t.Errorf("%d %s elements, want %d", count[element], element, want)
		}
	}

	if !strings.Contains(b.String(), `Text="Time &amp; delay"`) {
		t.Error("Parameter text is not escaped")
	}
}
