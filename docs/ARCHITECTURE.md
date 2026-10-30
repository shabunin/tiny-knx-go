# Architecture

tiny-knx-go is a protocol-level KNX stack for building KNX devices in Go, on Linux and on
microcontrollers (TinyGo). It is to Go what the Tasmota/esp-knx-ip and thelsing/OpenKNX stacks are to
Arduino, with the bus connection and the configuration source kept pluggable.

## Principles

1. **Protocol only in the core.** No `reflect`, no `net`, no third-party modules. Anything
   platform-specific (serial ports, sockets, flash) is injected.
2. **The bus connection is a plug-in.** Every medium implements `link.Link`. Media differ in how a
   frame gets confirmed (a BAOS module sends L_Data.con, a TP-UART reports the bus ACK, an IP
   router confirms nothing); the driver hides that, so `Send` means "confirmed by the bus"
   everywhere.
3. **Protocol state machines do no I/O.** The transport layer and the device runtime take events
   (frame received, time passed) and return frames to send. Timers are tested with a fake clock;
   the runner that connects them to a `Link` is a few lines.
4. **Configuration is data.** At runtime a device needs its individual address, its group objects
   and the group addresses linked to them. That is a plain Go struct, filled by code, a JSON file,
   a web UI, or (later) an ETS download. The core never knows which.

## Layers

```
 your device (switch actuator, sensor, gateway…)
 ─────────────────────────────────────────────────────────
 device     group objects, flags, prog mode,            done
            answers DeviceDescriptorRead / IA programming
 config     device.Config ← code | JSON | web UI | ets    done
 transport  T_Connect/T_Data/T_ACK state machine, Client done
 apdu       application services incl. 10-bit APCI       done
 dpt        datapoint encoding                            done
 cemi       cEMI frames, M_Prop*, M_Reset                 done
 knxnet     KNXnet/IP packets, tunnel client and server   done
            (no I/O)
 mgmt       line scan, property reader                    done
 baos       ObjectServer protocol 1.x/2.x (BAOS mode)     done
 ─────────────────────────────────────────────────────────
 link       Link interface                                done
   ft12       BAOS / kBerry over FT1.2 (cEMI LL mode)      done
   tpuart     NCN5120 / TP-UART2                           done (no hardware test)
   knxip      KNXnet/IP routing, tunnelling client/server  done (host only: uses net)
   mock       in-memory bus for tests and knxvbus          done
 ─────────────────────────────────────────────────────────
 port       io.ReadWriter: machine.UART (TinyGo), serial.Open (Linux), net.Conn
```

## ETS programming

ETS support lives in a separate package (`ets`), not in the core. `device` offers the building
blocks: interface objects with properties (`AddInterfaceObject`), memory services (`SetMemory`)
and `Bind`, which sets the flags and group addresses of the group objects. `ets.Attach` adds the
System B (0x07B0) objects of the thelsing stack: 1 group address table, 2 association table,
3 group object table, 4 application program. Each has the load state machine of
PID_LOAD_STATE_CONTROL with relative segment allocation; once all four are loaded, the tables are
turned into bindings and the application reads its parameters with `Param`. The image is persisted
through a `Storage` interface (flash on a microcontroller, a file on Linux). Devices that are
configured another way never link it.

The first ETS target is `knx-demo-tp.knxprod` from the thelsing stack (manufacturer 0x00FA,
application 0xABCE version 0x79). `ets/download_test.go` replays such a download on the mock bus;
`cmd/knxvbus -ets` hosts the device for a real ETS, which programs its address and downloads the
application. Seen in that download: ETS reads the description of PID_TABLE of the association
table to learn its format (PDT_GENERIC_04: TSAP and ASAP of 16 bits) and gives up without it;
it ignores HARDWARE_TYPE (zeros); it switches on verify mode through DEVICE_CONTROL and writes at
most 12 bytes per MemoryWrite (MAX_APDU_LENGTH 15); it unloads objects 5 to 1 (and accepts that
there is no object 5), starts loading and allocates the tables in the order 4, 3, 1, 2, writes
them, and completes them in the order 4, 3, 2, 1. A partial download unloads only the application
program, starts loading it, reads PID_TABLE_REFERENCE and writes the parameters there without
allocating the segment again, so unloading keeps the segment (the thelsing stack frees it and
fails here).
With several addresses per object, ETS writes one association per object first (its sending
address) and the other links after them, e.g. (1,1) (2,2) (3,3) (4,4) (5,2) (2,3), so the first
address found for an object is its sending address. Not done: master reset does not clear the tables, the priority in the group
object table is ignored.

## Roadmap

1. ✅ Port `cemi` and `dpt` from knx-go; TinyGo CI (host tests, QEMU tests, board builds).
2. ✅ `link` interface, FT1.2 / BAOS driver, Linux serial ports, `knxmon`, Pico + kBerry example.
3. ✅ `apdu`: typed encode/decode for the application services that devices and tools need
   (GroupValue*, IndividualAddress*, SerialNumber*, ADC*, Memory*, DeviceDescriptor*, Restart,
   Authorize/Key, PropertyValue*, PropertyDescription*); others decode as `apdu.Unknown`.
4. ✅ `transport`: the KNX 03_03_04 connection state machine (one `Conn` for both the device and the
   client side), `transport.Client` for tools, `link/mock`, `knxmon -info`.
5. ✅ `device`: group objects with C/R/W/T/U/I flags, `device.Config` (JSON), prog mode, IA
   programming (also by serial number), device object properties, DeviceDescriptorRead, Restart.
   First real device: switch actuator on Pico + kBerry.
6. ✅ `knxnet` (KNXnet/IP packets, search/description, tunnelling client state machine),
   `link/knxip` (tunnelling client with reconnect, routing with busy/rate handling),
   `link/tpuart` (TP-UART2, NCN5120/21/30), `knxmon -tunnel/-router/-tpuart`. A device takes over
   the address of a link that assigns one (tunnel).
7. ✅ Client tools: `mgmt.Scan` (parallel line scan on `transport.Conn`s, fast on acknowledged
   media), `mgmt.ReadProperties` (all interface objects, descriptions and values),
   `knxip.Search` (KNXnet/IP discovery), `knxmon -scan/-props/-search`.
8. ✅ ETS: `knxnet.Server` and `knxip.TunnelServer` (KNXnet/IP interface: search, extended search,
   description, tunnelling and device management connections), `cmd/knxvbus` (virtual line served
   to ETS), interface objects and memory services in `device`, `ets` (System B tables, load state
   machine, parameters, persistence). ETS finds the interface, programs the individual address and
   downloads the application of knx-demo-tp; the configuration survives a restart. Also tested
   with a replayed download and with Calimero/xknx as clients.
   `ets/product` describes a product once in Go for both sides: it writes the product database
   that OpenKNXproducer turns into a .knxprod and gives the firmware its group objects and
   parameter offsets. `knxvbus -sim` hosts a small installation built on it (`internal/vbus`:
   switch, dimmer, push buttons, thermostat, heating actuator, and rooms that the heating warms).
9. ✅ BAOS mode: `baos` (ObjectServer protocol 1.x and 2.x: server items, datapoint descriptions,
   values, commands, parameters, indications; version detection and switching from link layer
   mode), `knxmon -baos`. `link/ft12` restores the module's mode on `Close`. Tested with a kBerry
   (2.0) and a BAOS Module 820 (1.x).
10. ✅ `cmd/knxgw`: one gateway for all media (`internal/gateway`, shared with knxvbus). Link mode
    bridges any number of inputs (ft12, tpuart, tunnel, routing) and serves them as KNXnet/IP
    tunnelling servers, routing or a monitor; the serial module can be hosted as a device. BAOS
    mode serves the ObjectServer over TCP (port 12004, frames 06 20 F0 80) to several clients,
    each of which gets all indications, and connects to one as a client (`baos:tcp://`). Tested
    with a kBerry on a Raspberry Pi over Wi-Fi in both modes, and with a second gateway chained to
    it by tunnel that serves the line again by routing. `baos_auto` (`gateway.AutoModule`) keeps
    a module in ObjectServer mode and switches it to data link layer mode while a tunnel is open,
    like an IP BAOS; the ObjectServer is also served on a unix socket. Tested with the kBerry and
    the Serial BAOS 870: tunnel sessions of knxmon, then the ObjectServer back after the idle time,
    and the modules did not restart. `internal/sdk` serves the datapoints as JSON with the API of
    dobaos (`-o json:`, `cmd/baosctl`).
11. ✅ The ObjectServer over KNXnet/IP, like an IP BAOS: `knxnet.Server` serves connections of type
    0xF0 (ObjectServer requests 0xF080 and acknowledgements 0xF081, header version 2.0) next to
    tunnels, and search responses carry the service family 0xF0 and the manufacturer DIB (0xFE,
    00C5, record 01 04 F0 and the version, 0x10 or 0x20). Without tunnels a server announces
    three families, so that the DIB comes at offset 76 as the ObjectServer protocol shows it.
    `knxnet.Tunnel` opens such connections too, and `knxip.DialObjectServer` is a
    `baos.Transport` over them (`baos:udp://`, `knxmon -port udp://HOST -baos`). A request in the
    other protocol version than the module's gets "service not supported" at once instead of the
    silence of a module, so that clients detect the version without waiting for a timeout.
    `knxgw` serves the ObjectServer at its knxip outputs in BAOS and auto mode. Tested with the
    BAOS Module 820 (1.x) through `knxgw -o knxip`: found by `knxmon -search`, datapoints read
    over UDP. Not yet tested with a real IP BAOS or its clients; the version of the service family
    (the major protocol version) is a guess.

## Hardware notes

- BAOS modules (kBerry = BAOS 838): 19200 baud, 8 data bits, even parity, 1 stop bit. The driver
  switches the module to data link layer mode by writing `PID_COMM_MODE` (52) = 0 on the cEMI
  server object, and does it again after the module resets. `Close` writes back the mode it found,
  usually 0xF0, the BAOS (ObjectServer) mode; the Weinzierl SDK names the modes Link 0x00,
  Busmonitor 0x01, Raw 0x02, 0x06, 0xF0 and None 0xFF. In link layer mode the module's own stack
  does not answer on the bus (no DeviceDescriptorResponse to ETS) and it ignores ObjectServer
  requests; in BAOS mode it answers ETS as a device (mask 07B0, order number "838"). From the
  serial side, its stack does answer in link layer mode: an L_Data.req to its own address (source
  0.0.0, which it fills in) comes back as L_Data.ind with source and destination both its address,
  e.g. `11 00 b0 60 0000 1401 01 43 00` (DeviceDescriptorRead) → `29 00 b0 60 1401 1401 03 43 40 07
  b0`, and broadcasts it sends are answered too (IndividualAddressResponse in programming mode,
  PID_PROG_MODE 54). That is how ETS programs a kBerry through a tunnel on that kBerry (as
  dobaosll's llnet did). The BAOS 820 over EMI2 does not: T_Connect to itself is not confirmed.
- BAOS Module 820 (RS-232, BCU2 mask 0701): EMI2 for link layer mode, ObjectServer protocol 1.x in
  normal mode on the same FT1.2 line; each version ignores requests of the other, which is how
  `baos.NewClient` detects it. It has 250 datapoints and a 240-byte buffer, and reports the first ID
  it could not take in a negative response (buffer too small at the 79th description, bad ID 251).
  It ignores PC_GetValue/PC_SetValue, so its individual address is assigned over the bus, e.g.
  `knxmon -serial-address`; `SetAddress` returns `ErrAddressIgnored` (checked on the module).
  Version 2 (kBerry) rejects server item ranges that start after its last item (17) and answers
  "no element" for datapoint ranges without configured datapoints.
- TinyGo's `machine.UART.Read` returns immediately when no data is buffered; the FT1.2 driver
  polls it every millisecond.
- Frames we originate use control field 0xBC (low priority), the value ETS uses
  (`cemi.Control1Request`).
- A BAOS module occupies one individual address and only acknowledges and forwards point-to-point
  frames for it. A device on a kBerry therefore uses the module's address: `device.Run` sets it
  through `ft12.Link.SetAddress`, which writes `PID_SUBNET_ADDR` (57) and `PID_DEVICE_ADDR` (58) of
  the device object, and only when the address differs. Checked on a kBerry: the address changes
  and survives reopening the module.
- KNXnet/IP was tested against Calimero server 3.0 (tunnelling, and routing as a KNX IP router)
  and xknx 4.0 (routing): group telegrams both ways, and `knxmon -info` through a Calimero tunnel to
  a `device` on routing. The tunnel uses route-back HPAIs (0.0.0.0:0), which works through NAT.
- Line scans: on TP (directly, through a BAOS module or a tunnel to a TP interface) an absent device
  costs one unacknowledged T_Connect. Without acknowledgements (routing, Calimero's virtual subnet)
  every address waits for the transport timeouts; 256 addresses take about 30 s with 16 probes in
  parallel.
- Calimero server 3.0-M2 quirk: after a client has read the association table object of
  Calimero's own device (7.1.0), Calimero stops forwarding that tunnel address's frames to other
  devices. Seen with `knxmon -props 7.1.0`; a new tunnel address works again.
- Go's `net.ListenMulticastUDP` disables multicast loopback, so `knxip.Router` sends from a second
  socket; otherwise programs on the same host would not see each other's frames.
- TP-UART: no hardware available yet. Behaviour follows the thelsing stack (echo filtering, no
  acknowledgement while sending, U_SetAddress on both chips). By default every group telegram is
  acknowledged; set `tpuart.Config.AcceptGroup` to acknowledge only the device's group addresses.
- `knxip.TunnelServer` follows the thelsing stack: tunnel connections (link layer only) and
  device management connections, whose M_PropRead requests are answered from the server
  description (serial number, address, tunnel addresses, name, MAC). Group frames go to every tunnel
  except the sender, individual frames only to the tunnel with that address. Searches are answered
  on the routing multicast group, extended searches (KNXnet/IP 2, ETS 6) with an extended response.
  The control socket uses SO_REUSEADDR so that it shares port 3671 with routing participants on
  the same host. Tested with `knxmon` (search, scan, info, group writes), xknx 4.0 (gateway scan,
  tunnel) and Calimero 3.0 as a client (IA programming of a device in programming mode, line scan).
  On the in-memory bus a device answers before `Send` returns, so each tunnel holds back received
  frames until the L_Data.con of its own frame is out; Calimero ignores a T_ACK that arrives before
  the confirmation.
- Linux pseudo-terminals ignore parity, so `socat` pty pairs work for testing without hardware.
