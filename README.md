# tiny-knx-go

KNX protocol packages for Go and [TinyGo](https://tinygo.org): build and parse cEMI frames and
datapoint values with no dependencies, on a server or on a microcontroller.

Derived from [vapourismo/knx-go](https://github.com/vapourismo/knx-go) (MIT). The design and
roadmap are in [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Packages

 Package | Description
---------|------------------------------------------------------------------
 `cemi`  | cEMI frames: L_Data, L_Raw, L_Busmon, addresses, control fields, TPDU/APCI
 `dpt`   | Datapoint types (DPT 1, 3, 5–14, 16–18, 20, 28, 232, 242, 251)
 `apdu`  | Application layer services (group values, properties, memory, device descriptor…)
 `transport` | Transport connections: state machine without I/O, and a Client for tools
 `device` | A KNX device: group objects with flags, programming mode, individual address programming, interface objects and properties, memory services
 `link`  | Interface between the protocol stack and a bus connection
 `link/mock` | In-memory bus for tests
 `link/ft12` | BAOS modules (kBerry) over FT1.2 serial, in cEMI data link layer mode
 `link/tpuart` | TP-UART2 and NCN5120/5121/5130 transceivers
 `knxnet` | KNXnet/IP packets and the tunnelling client and server state machines, without I/O, with KNX IP Secure sessions
 `secure` | KNX Data Secure (A_SecureService, AES-128 CCM, sync requests) and the cryptography of KNX IP Secure
 `link/knxip` | KNXnet/IP tunnelling client and server, routing and discovery over UDP (uses `net`)
 `ets` | Makes a device programmable by ETS as a System B device (0x07B0): table objects, load state machine, memory, parameters, persistence
 `ets/product` | Describes the ETS product of a device in Go: writes its product database (XML for a .knxprod) and gives the firmware its group objects and parameters
 `mgmt` | Management procedures for tools: line scan, reading all properties of a device
 `serial` | Opens serial ports on Linux with the standard library only
 `cmd/knxmon` | Bus tool over kBerry, TP-UART, KNXnet/IP tunnel or routing: monitor, group reads/writes, line scan, device properties, KNXnet/IP search
 `cmd/knxvbus` | Virtual KNX line with devices, served as a KNXnet/IP interface that ETS finds and tunnels to; simulated switch, dimmer, push buttons, thermostat and heating
 `examples/pico-kberry` | Switch actuator: Pico + kBerry, LED on 1/1/1, status on 1/1/2, programming button

The core packages use no `reflect`, no `net` and no third-party modules. Only `link/knxip`,
`serial` and the commands use the operating system.

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
./knxmon -tunnel 192.168.1.10                   # through a KNXnet/IP interface or router
./knxmon -router -iface eth0 -address 1.1.254   # KNXnet/IP routing (multicast 224.0.23.12)
./knxmon -port /dev/ttyUSB0 -tpuart             # TP-UART / NCN5120 (-chip tpuart2 for Siemens)
./knxmon -search                                # find KNXnet/IP interfaces and routers
./knxmon -port /dev/serial0 -scan 1.1           # list the devices of line 1.1
./knxmon -port /dev/serial0 -props 1.1.3        # dump all interface objects and properties
```

On a Pico wired to the kBerry (GP0 → kBerry RX, GP1 ← kBerry TX, 3V3, GND; optional programming
button GP15 → GND and LED on GP14):

```sh
tinygo flash -target=pico ./examples/pico-kberry
```

## A virtual bus for ETS

`knxvbus` runs devices on an in-memory line and serves it as a KNXnet/IP interface on the LAN.
ETS (or `knxmon -tunnel`, Calimero, xknx) finds it by search and connects by tunnelling; each
tunnel is a participant of the line with its own address (1.1.250–253 by default).

```sh
go run ./cmd/knxvbus -iface eth0 -devices 1.1.1 -ets 15.15.255 -monitor
```

Type `p 2` and Enter to press the programming button of device 2, `l` to list the devices.
`-trace` prints every KNXnet/IP packet. `-iface` must be the interface on the network of the ETS
computer: the server answers connections only on its address there.

Devices given with `-ets` are the "Temperatursensor TP" of the thelsing stack's
`examples/knx-demo/knx-demo-tp.xml`: import it into ETS, program the individual address and
download the application. Their configuration is saved in `knxvbus-data/`.

Current ETS versions refuse to import the `.knxprod` that comes with the thelsing stack ("No valid
license found to test the unregistered product(s)"). Mark the product and its application as
registered in the XML, as the OpenKNX templates do, and convert it with
[OpenKNXproducer](https://github.com/OpenKNX/OpenKNXproducer) on the ETS computer, which signs it
with the DLLs of the installed ETS:

```xml
<Product Id="M-00FA_H-0124-0_P-IA4313" ...>
  <RegistrationInfo RegistrationStatus="Registered" />
</Product>
...
<Hardware2Program Id="M-00FA_H-0124-0_HP-ABCE-79-0000" MediumTypes="MT-0">
  <ApplicationProgramRef RefId="M-00FA_A-ABCE-79-0000" />
  <RegistrationInfo RegistrationStatus="Registered" RegistrationNumber="0001/0121" />
</Hardware2Program>
```

```
OpenKNXproducer-x64.exe knxprod -o knx-demo-tp.knxprod knx-demo-tp.xml
```

### A simulated installation

`-sim` adds devices that ETS programs from their own products: a 2-fold switch actuator with
staircase function, a dimmer, a 4-fold push button, a room thermostat (PI or two-point) and a
heating actuator. Heating actuators warm one of four simulated rooms, thermostats measure it; with
all heaters off a room cools down to 5 °C, 2000 W keep it at 30 °C. `-speed` runs the rooms faster
than real time (5 by default).

```sh
go run ./cmd/knxvbus -xml products        # TKGSA2.xml, TKGDA1.xml, TKGPB4.xml, TKGRT1.xml, TKGHA1.xml
go run ./cmd/knxvbus -iface eth0 -sim switch,dimmer,buttons,thermostat,heater -monitor
```

Convert the XMLs with OpenKNXproducer on the ETS computer and import the `.knxprod` files (catalog
section "tiny-knx-go"). Every simulated device starts at 15.15.255, or at the address given as
`dimmer@1.1.20`; program them one by one with `p N`. Parameters and group objects are those of
`internal/vbus/products.go`; a device keeps its defaults until ETS downloads its application.

Commands for the simulation:

```
l                  devices with their state, and the room temperatures
b 3 1              short press of button 1 of device 3
b 3 2 long 1.5     hold button 2 for 1.5 s (dims if its function is dimming)
t 1 17             set room 1 to 17 °C, e.g. after opening a window
```

### KNX IP Secure

With `-password`, the interface needs a secure session (KNX IP Secure, over UDP) for tunnelling and
device management, and rejects plain connections. The management user 1 and one tunnelling user
per tunnel (2 and up) share the password; `-auth` sets the device authentication code with which
the interface proves itself.

```sh
go run ./cmd/knxvbus -iface eth0 -sim switch -password tinyknx -auth tinyauth
go run ./cmd/knxmon -tunnel 192.168.1.10 -password tinyknx -auth tinyauth -write 1/0/1=1
```

`secure` is checked against the examples of AN158 and AN159 and against telegrams of an ETS project
that xknx and calimero test with.

## Making a device programmable by ETS

```go
d, _ := device.New(device.Identity{SerialNumber: serial, OrderNumber: "IA4313"}, device.Config{
	Objects: []device.ObjectConfig{{Name: "current", DPT: "9.001"}, {Name: "max", DPT: "9.001"},
		{Name: "min", DPT: "9.001"}, {Name: "reset", DPT: "1.017"}}, // ComObject numbers 1–4
})
e, _ := ets.Attach(d, storage) // storage keeps the download, e.g. in flash or a file
e.OnConfigured(func() { delay := e.Param(0); _ = delay })
d.Run(l)
```

ETS writes the group addresses, flags and parameters; the firmware defines the group objects in
the order of the product database. The serial number must start with the manufacturer ID of the
product.

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
