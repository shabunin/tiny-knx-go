module github.com/shabunin/tiny-knx-go/examples/cardputer

go 1.24.4

replace github.com/shabunin/tiny-knx-go => ../..

require (
	tinygo.org/x/drivers v0.37.0
	tinygo.org/x/espradio v0.5.0
	tinygo.org/x/tinyfont v0.7.0
)

require github.com/soypat/lneto v0.4.0 // indirect

require (
	github.com/google/shlex v0.0.0-20191202100458-e7afc7fbc510 // indirect
	github.com/shabunin/tiny-knx-go v0.0.0
)
