# tiny-knx-go

KNX protocol packages for Go and [TinyGo](https://tinygo.org): build and parse cEMI frames and
datapoint values with no dependencies, on a server or on a microcontroller.

Derived from [vapourismo/knx-go](https://github.com/vapourismo/knx-go) (MIT). The design and
roadmap are in [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Packages

✓ marks the packages whose tests also run under TinyGo on an emulated microcontroller
(`make check`).

**Protocol core**

| Package | Description | TinyGo |
| :--- | :--- | :---: |
| `cemi` | cEMI frames: L_Data, L_Raw, L_Busmon, property services, addresses, control fields, TPDU/APCI | ✓ |
| `apdu` | Application layer services: group values, properties, memory, device descriptor… | ✓ |
| `transport` | Transport connections: a state machine without I/O, and a client for tools | ✓ |
| `dpt` | Datapoint types (DPT 1, 3, 5–14, 16–18, 20, 28, 232, 242, 251) | ✓ |
| `knxnet` | KNXnet/IP packets, the tunnelling client and server state machines without I/O, KNX IP Secure sessions | ✓ |
| `secure` | KNX Data Secure (A_SecureService, AES-128 CCM, sync requests) and the cryptography of KNX IP Secure | ✓ |

**Devices and ETS**

| Package | Description | TinyGo |
| :--- | :--- | :---: |
| `device` | A KNX device: group objects with flags, programming mode, individual address programming, interface objects and properties, memory services | ✓ |
| `ets` | Makes a device programmable by ETS as a System B device (07B0): table objects, load state machine, memory, parameters, persistence | ✓ |
| `ets/product` | Describes the ETS product of a device in Go: writes its product database (XML for a .knxprod) and gives the firmware its group objects and parameters | ✓ |
| `mgmt` | Management procedures for tools: line scan, reading all properties of a device | ✓ |

**Links to the bus**

| Package | Description | TinyGo |
| :--- | :--- | :---: |
| `link` | Interface between the protocol stack and a bus connection | ✓ |
| `link/mock` | In-memory bus for tests | ✓ |
| `link/ft12` | BAOS modules (kBerry) and older EMI2 interfaces over FT1.2 serial, in data link layer mode; the protocol is detected, module restarts are survived | ✓ |
| `link/tpuart` | TP-UART2 and NCN5120/5121/5130 transceivers | ✓ |
| `link/knxip` | KNXnet/IP tunnelling client and server, routing and discovery over UDP and TCP (uses `net`) | |
| `baos` | BAOS mode: the ObjectServer protocol (versions 1 and 2) of Weinzierl modules, whose datapoints ETS configures | ✓ |
| `serial` | Opens serial ports on Linux with the standard library only | |

**Commands** (Linux)

| Command | Description |
| :--- | :--- |
| `cmd/knxmon` | Bus tool over kBerry, TP-UART, KNXnet/IP tunnel or routing: monitor, group reads/writes, line scan, device properties and memory, KNXnet/IP search; `-baos` talks to an ObjectServer on a serial port, `tcp://` or `unix:` |
| `cmd/knxgw` | Gateway between media, like ffmpeg: inputs kBerry/EMI2, TP-UART, tunnel, routing; outputs KNXnet/IP interface, routing, monitor. BAOS mode serves the ObjectServer over TCP or a unix socket like an IP BAOS, and the datapoints as JSON like dobaos; `baos_auto` switches a module between both modes |
| `cmd/baosctl` | REPL for the JSON datapoints of knxgw, a port of dobaos.tool, with history, line editing and completion |
| `cmd/knxvbus` | Virtual KNX line with devices, served as a KNXnet/IP interface that ETS finds and tunnels to; simulated switch, dimmer, push buttons, thermostat and heating |

**Internal**, shared by the commands

| Package | Description |
| :--- | :--- |
| `internal/gateway` | The media of knxgw and knxvbus: specs, opening lines, bridging, the BAOS hub, the ObjectServer over TCP and unix sockets, switching a module between modes |
| `internal/sdk` | The JSON API of the datapoints: server, client, values by DPT, names |
| `internal/vbus` | The simulated installation of knxvbus |
| `internal/util`, `internal/pretty` | Helpers; printing telegrams for monitors |

**Examples**

| Path | Description |
| :--- | :--- |
| `examples/pico-kberry` | Switch actuator: Pico + kBerry, LED on 1/1/1, status on 1/1/2, programming button |
| `examples/smoke` | Encodes a frame and a value; `make check` builds it for the Pico, ESP32 and ESP32-C3 and runs it on an emulated Cortex-M |

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
./knxmon -port /dev/serial0 -progmode           # list the devices whose programming button is on
./knxmon -port /dev/serial0 -key ffffffff -memory 1.1.3:4000:40   # dump memory (authorized)
./knxmon -port /dev/ttyUSB0                     # an older EMI2 interface (RS-232 BAOS, BCU2)
./knxmon -port /dev/serial0 -baos               # BAOS mode: the module's datapoints and values
./knxmon -port /dev/serial0 -baos -set 2=01 -get 1   # set and send DP 2, read DP 1 from the bus
```

A BAOS module has two modes. In data link layer mode it passes telegrams, which is what `knxmon`,
`knxvbus -serial` and devices on a kBerry use; the module's own KNX stack then stays silent on the
bus. In BAOS mode the module is a device of its own: ETS loads its application, which links its
datapoints to group addresses, and a client reads and writes datapoints through the ObjectServer
protocol (package `baos`, `knxmon -baos`). Links switch the module to data link layer mode and back
to the mode it was in when they close (knxmon does so on Ctrl-C): kBerry modules through the cEMI
communication mode (0x00 link layer, 0xF0 BAOS), EMI2 modules (BAOS 820) with PEI_Switch.

On a Pico wired to the kBerry (GP0 → kBerry RX, GP1 ← kBerry TX, 3V3, GND; optional programming
button GP15 → GND and LED on GP14):

```sh
tinygo flash -target=pico ./examples/pico-kberry
```

## A gateway between media

`knxgw` connects inputs, which reach a real line, to outputs, which serve it, like ffmpeg does with
streams. In link mode the inputs pass telegrams, are bridged to each other and serve as one line:

```sh
knxgw -i ft12:/dev/ttyAMA0 -o knxip:wlan0                   # kBerry as a KNXnet/IP interface
knxgw -i ft12:/dev/ttyUSB0?device -o knxip:eth0 -o monitor  # an EMI2 module, hosted as a device
knxgw -i 'tpuart:/dev/ttyUSB0?address=1.1.250' -o knxip:eth0 -o routing:eth0
knxgw -i tunnel:192.168.1.112 -o 'knxip:wlan0?port=3672&name=via the Pi'
knxgw -i routing:eth0 -o 'knxip:eth0?tunnels=1.1.250,1.1.251'  # tunnelling for a KNX IP router
```

In BAOS mode the input is the ObjectServer of a BAOS module, and `-o baos` serves it over TCP port
12004 like an IP BAOS, to any number of clients; all of them get the value indications:

```sh
knxgw -i baos:/dev/ttyAMA0 -o baos:wlan0 -o monitor   # on the Pi
knxgw -i baos:tcp://192.168.2.29 -o monitor           # anywhere else
```

`-o baos:/PATH` serves the same frames on a unix socket, for programs on the same host.

`baos_auto` makes a module both, like an IP BAOS: it stays in ObjectServer mode until ETS opens a
tunnel of a knxip output, switches to data link layer mode for the tunnel, and goes back to
ObjectServer mode 5 seconds (`?idle=`) after the last tunnel closed. While ETS has the line, the
ObjectServer clients are disconnected and new ones turned away; `p` switches the programming mode
in either mode (server item 15 in ObjectServer mode).

```sh
knxgw -i baos_auto:/dev/ttyAMA0 -o knxip:wlan0 -o baos:wlan0 -o baos:/run/knxgw.sock
```

### Datapoints as JSON, like dobaos

`-o json:/PATH` serves the datapoints on a unix socket with the JSON API of
[dobaos](https://github.com/cougalix/dobaos), without Redis: one JSON object per line each way,
and the response repeats the `id` of the request. Values are converted by their DPT, in the formats
of dobaos (DPT 1 `true`, DPT 9 `21.5`, DPT 10 `{"day":1,"hour":12,"minutes":30,"seconds":0}`, ...),
with the data in base64 as `raw`. `?names=FILE` keeps names of datapoints (`{"room_temperature": 8}`),
which requests take instead of IDs; the methods `set name`, `get name` and `remove name` change it.

```text
-> {"id": 1, "method": "get value", "payload": [1, "room_temperature"]}
<- {"id": 1, "method": "success", "payload": [{"id": 1, "value": true, "raw": "AQ=="}, {"id": 8, "value": 21.5, "raw": "DDM="}]}
<- {"method": "datapoint value", "payload": [{"id": 8, "value": 21.6, "raw": "DDg="}]}
```

| method | payload | |
| :--- | :--- | :--- |
| `get description` | `null`/`"*"`, ID, name or array | type (`dpt9`), size in bits, priority, flags, name |
| `get value` | the same | values read from the module |
| `get stored` | the same | values as last known, without asking the module |
| `set value` | `{"id", "value"}` or `{"id", "raw"}`, or an array | set and send to the bus |
| `put value` | the same | set without sending |
| `read value` | IDs | send GroupValueRead; the responses come as broadcasts |
| `get programming mode`, `set programming mode` | `true`/`false` | |
| `get name`, `set name`, `remove name` | ID or name; `{"id", "name"}`; IDs or names | |
| `get server items`, `version`, `reset` | | `reset` reloads the datapoints |

Errors are dobaos's codes, e.g. `{"id": 2, "method": "error", "payload": "ERR_DATAPOINT_NOT_FOUND"}`;
in `baos_auto` mode, while a tunnel is open, `ERR_NO_OBJECT_SERVER`. Every client gets the
broadcasts `datapoint value` (from the bus, and after `set value`/`put value`) and `server item`.

`baosctl` is a REPL for it, a Go port of dobaos.tool with its commands and output: `watch 1: red`
colors a datapoint's values (or hides them), `name 8: temperature` names one (kept in the names
file). It has history (up, down, kept in `~/.config/baosctl/history`), line editing and completion
of commands (tab). With arguments it runs one command, e.g. `baosctl get 1 2`.

```text
$ baosctl -s /run/knxgw.json
baos> description *
#8 <temperature>, dpt9, 16 bits, prio: low, flags: [C-WT-]
baos> set [temperature: 22.5, 1: true]
01:01:30.706,    id: 8 <temperature>, value: 22.5, raw: [12,101] [0x0c65]
baos> set 9: {day: 1, hour: 12, minutes: 30, seconds: 0}
baos> raw 6: 0x1234
baos> get *
baos> progmode true
```

ETS reaches a kBerry itself through the tunnel of its address: the module answers frames to its
own address from the serial side, in data link layer mode too. So ETS can assign its address and
download its BAOS application through the gateway on that very module; `p` on standard input
switches its programming mode (PID_PROG_MODE of its device object). An EMI2 module (BAOS 820) does
not answer that way, and `?device` hosts a device with its mask and serial number for it instead.

`knxgw -h` lists all media and their options. Inputs must reach different lines, or frames loop;
routing ignores the echo of its own multicast, so it can be both an input and an output.

## A virtual bus for ETS

`knxvbus` runs devices on an in-memory line and serves it as a KNXnet/IP interface on the LAN.
ETS (or `knxmon -tunnel`, Calimero, xknx) finds it by search and connects by tunnelling; each
tunnel is a participant of the line with its own address (1.1.250–253 by default).

```sh
go run ./cmd/knxvbus -iface eth0 -devices 1.1.1 -ets 15.15.255 -monitor
```

Type `p 2` and Enter to press the programming button of device 2, `l` to list the devices.

With `-serial`, the virtual line is connected to a real one through a kBerry or an EMI2 interface,
which makes `knxvbus` a KNXnet/IP gateway to the real bus. The module only acknowledges frames to
its own address, so the tunnel gets that address:

```sh
knxvbus -iface eth0 -serial /dev/ttyAMA0 -devices '' -monitor   # on a Raspberry Pi with a kBerry
knxmon -tunnel 192.168.1.112 -props 1.1.4                       # a real device, from another machine
```

With `-tunnel`, the line is connected through a tunnel to another interface instead, for example to
reach that Raspberry Pi from a network that only this machine can reach. Several `knxvbus` can run
on one machine with different `-port`s; ETS finds them all.

```sh
knxvbus -iface wlan0 -port 3672 -tunnel 192.168.1.112 -devices '' -name "real bus via the Pi"
```

`-module-device` hosts the module itself as device 1 with its mask version and serial number, so
that ETS can assign it an address through another interface on the line: `p 1` switches its
programming mode, and the new address is written to the module. EMI2 modules that refuse the
memory write get their address over the bus instead, by serial number:

```sh
knxmon -tunnel 192.168.1.112 -serial-address 00c501004122:1.4.2
```

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

With `-password`, the interface needs a secure session (KNX IP Secure) for tunnelling and device
management, and rejects plain connections. knxvbus takes connections over UDP and TCP; ETS opens
secure sessions only over TCP, and authenticates as the management user 1. The management user 1 and one tunnelling user
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
