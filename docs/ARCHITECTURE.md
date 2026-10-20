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
 device     group objects, flags, prog mode,            planned
            answers DeviceDescriptorRead / IA programming
 config     device.Config ← code | JSON | web UI | ets    planned
 transport  T_Connect/T_Data/T_ACK state machines        planned
 apdu       all application services incl. 10-bit APCI   planned
 dpt        datapoint encoding                            done
 cemi       cEMI frames, M_Prop*, M_Reset                 done
 ─────────────────────────────────────────────────────────
 link       Link interface                                done
   ft12       BAOS / kBerry over FT1.2 (cEMI LL mode)      done
   tpuart     NCN5120 / TP-UART2                           planned
   knxip      KNXnet/IP routing + tunnelling client        planned
   mock       in-memory bus for tests                      planned
 ─────────────────────────────────────────────────────────
 port       io.ReadWriter: machine.UART (TinyGo), serial.Open (Linux), net.Conn
```

## ETS programming

ETS support lives in a separate module (`ets`), not in the core. It implements the interface objects,
memory, load-state machine and property services that ETS talks to, and turns a completed download
into the same `device.Config`, persisted through a `Storage` interface (flash on a microcontroller,
a file on Linux). Devices that are configured another way never link it.

## Roadmap

1. ✅ Port `cemi` and `dpt` from knx-go; TinyGo CI (host tests, QEMU tests, board builds).
2. ✅ `link` interface, FT1.2 / BAOS driver, Linux serial ports, `knxmon`, Pico + kBerry example.
3. `apdu`: typed encode/decode for every application service (PropertyValue*, Memory*,
   DeviceDescriptor*, IndividualAddress*, Restart, Authorize…).
4. `transport`: connection-oriented state machines, device side and client side; `link/mock`.
5. `device`: group objects with C/R/W/T/U flags, `device.Config`, prog mode, management responses.
   First real device: switch actuator on Pico + kBerry.
6. `link/knxip` (routing for Linux/Pi, tunnelling client) and `link/tpuart`.
7. Client tools: line scanner, property reader.
8. `ets` module: ETS-programmable devices.

## Hardware notes

- BAOS modules (kBerry = BAOS 838): 19200 baud, 8 data bits, even parity, 1 stop bit. The driver
  switches the module to data link layer mode by writing `PID_COMM_MODE` (52) = 0 on the cEMI
  server object, and does it again after the module resets.
- TinyGo's `machine.UART.Read` returns immediately when no data is buffered; the FT1.2 driver
  polls it every millisecond.
- Linux pseudo-terminals ignore parity, so `socat` pty pairs work for testing without hardware.
