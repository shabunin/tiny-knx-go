// Command smoke builds and parses a KNX group telegram. It exists so that CI can check that the
// core packages compile for microcontrollers (tinygo build) and run on an emulated one
// (tinygo run -target=cortex-m-qemu).
package main

import (
	"github.com/shabunin/tiny-knx-go/cemi"
	"github.com/shabunin/tiny-knx-go/dpt"
)

const hexDigits = "0123456789abcdef"

func toHex(data []byte) string {
	out := make([]byte, 0, len(data)*2)
	for _, b := range data {
		out = append(out, hexDigits[b>>4], hexDigits[b&15])
	}

	return string(out)
}

func main() {
	// Write 20.5 °C (DPT 9.001) to group 1/2/3.
	req := &cemi.LDataReq{LData: cemi.LData{
		Control1:    cemi.Control1StdFrame | cemi.Control1Prio(cemi.PrioLow),
		Control2:    cemi.Control2GroupAddr | cemi.Control2Hops(6),
		Destination: uint16(cemi.NewGroupAddr3(1, 2, 3)),
		Data: &cemi.AppData{
			Command: cemi.GroupValueWrite,
			Data:    dpt.DPT_9001(20.5).Pack(),
		},
	}}

	frame := make([]byte, cemi.Size(req))
	cemi.Pack(frame, req)
	println("frame:", toHex(frame))

	var msg cemi.Message
	if _, err := cemi.Unpack(frame, &msg); err != nil {
		println("unpack failed:", err.Error())
		return
	}

	app := msg.(*cemi.LDataReq).Data.(*cemi.AppData)

	var temp dpt.DPT_9001
	if err := temp.Unpack(app.Data); err != nil {
		println("dpt unpack failed:", err.Error())
		return
	}

	println("value:", temp.String())
	println("OK")
}
