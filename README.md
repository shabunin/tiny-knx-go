# tiny-knx-go

KNX protocol packages for Go and [TinyGo](https://tinygo.org): build and parse cEMI frames and
datapoint values with no dependencies, on a server or on a microcontroller.

Derived from [vapourismo/knx-go](https://github.com/vapourismo/knx-go) (MIT). The design and
roadmap are in [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Packages

 Package | Description
---------|------------------------------------------------------------------
 `cemi`  | cEMI frames: L_Data, L_Raw, L_Busmon, addresses, control fields, TPDU/APCI
 `dpt`   | Datapoint types (DPT 1, 5–14, 16–18, 20, 28, 232, 242, 251)
 `apdu`  | Application layer services (group values, properties, memory, device descriptor…)
 `transport` | Transport connections: state machine without I/O, and a Client for tools
 `device` | A KNX device: group objects with flags, programming mode, individual address programming by ETS
 `link`  | Interface between the protocol stack and a bus connection
 `link/mock` | In-memory bus for tests
 `link/ft12` | BAOS modules (kBerry) over FT1.2 serial, in cEMI data link layer mode
 `serial` | Opens serial ports on Linux with the standard library only
 `cmd/knxmon` | Bus monitor for a kBerry on a Raspberry Pi: prints telegrams, sends reads/writes, reads device identity
 `examples/pico-kberry` | Switch actuator: Pico + kBerry, LED on 1/1/1, status on 1/1/2, programming button

The core packages use no `reflect`, no `net` and no third-party modules.

## Differences from knx-go

- `dpt.Produce` uses constructor functions instead of `reflect`.
- The ISO 8859-1 string helpers (and with them `golang.org/x/text`) are gone; nothing in `cemi` or
  `dpt` used them.
- Fixed: a TPDU shorter than its length byte was accepted and zero-padded, while a frame with
  bytes after the TPDU was rejected.
- Fixed: `ControlField2.Hops()` read the wrong bits.
- Fixed: `LRawInd.MessageCode()` returned the L_Raw.con code.
- Added: cEMI management messages (M_PropRead, M_PropWrite, M_PropInfo, M_Reset).

## Trying it with a kBerry

On a Raspberry Pi with the kBerry on the GPIO header (serial console disabled, UART enabled):

```sh
GOOS=linux GOARCH=arm64 go build ./cmd/knxmon   # GOARCH=arm GOARM=6 for 32-bit Raspberry Pi OS
./knxmon -port /dev/serial0                     # watch the bus
./knxmon -port /dev/serial0 -write 1/1/1=1      # switch something on
./knxmon -port /dev/serial0 -info 1.1.3         # connect to a device, read mask, serial, order no.
./knxmon -port /dev/serial0 -address 1.1.250    # change the kBerry's own individual address
```

On a Pico wired to the kBerry (GP0 → kBerry RX, GP1 ← kBerry TX, 3V3, GND; optional programming
button GP15 → GND and LED on GP14):

```sh
tinygo flash -target=pico ./examples/pico-kberry
```

## Writing a device

```go
d, _ := device.New(device.Identity{SerialNumber: serial, OrderNumber: "MY-SWITCH"}, device.Config{
	Objects: []device.ObjectConfig{
		{Name: "switch", DPT: "1.001", Flags: device.FlagCommunicate | device.FlagWrite,
			Addresses: []cemi.GroupAddr{cemi.NewGroupAddr3(1, 1, 1)}},
		{Name: "status", DPT: "1.001", Flags: device.FlagCommunicate | device.FlagRead | device.FlagTransmit,
			Addresses: []cemi.GroupAddr{cemi.NewGroupAddr3(1, 1, 2)}},
	},
})

d.Object("switch").OnChange(func(o *device.Object) {
	var on dpt.DPT_1001
	o.Get(&on)
	relay.Set(bool(on))
	d.Object("status").Set(on) // sent to 1/1/2
})

d.Run(l) // l from e.g. ft12.Open(uart, ft12.DefaultConfig)
```

`device.Config` is plain data with JSON tags (`"address": "1.1.50"`, `"flags": "CWT"`,
`"addresses": ["1/1/1"]`). Save `d.Config()` in `OnAddressChange` to keep an address assigned by ETS.

## Development

```sh
make test          # go vet + go test on the host
make tinygo-test   # unit tests on emulated Cortex-M and RISC-V (tinygo, qemu-system-arm/-riscv32)
make tinygo-build  # firmware for pico, esp32-coreboard-v2 and xiao-esp32c3
make check         # all of the above
```
