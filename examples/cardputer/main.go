//go:build tinygo

// Command cardputer is a pocket KNX tool on the M5Stack Cardputer (ESP32-S3): it opens a tunnel to
// a KNXnet/IP interface over Wi-Fi, shows the bus on its screen and sends what is typed on its
// keyboard:
//
//	w 1/1/1 on      group write: on, off, 0-255, 21.5 (DPT 9), 50% (DPT 5.001), x0c1a (raw)
//	r 1/1/2         group read
//	clear
//
// The interface must take tunnelling data at its control endpoint, as most do (TinyGo's UDP
// sockets are connected). Commands can also be typed on the USB serial console.
//
// The settings are kept in flash and changed on the keyboard, then saved and applied by a
// restart:
//
//	ssid NAME, wifipass PW, gw HOST[:PORT]   Wi-Fi and the KNXnet/IP interface
//	user 2, knxpass PW, auth CODE            KNX IP Secure tunnel ("-" clears)
//	show, save, reboot
//
// The settings can also be given at build time; a build with other settings replaces those in
// flash once:
//
//	tinygo flash -target=m5stamp-s3a -port /dev/ttyACM0 \
//	  -ldflags "-X main.ssid=SSID -X main.password=PASSWORD -X main.gateway=192.168.1.10" .
//
// A secure tunnel also takes -X main.knxPassword=..., and optionally -X main.knxUser=2 and
// -X main.knxAuth=... (the device authentication code). Only the keys derived from the KNX
// passwords are saved, so they are computed once (about 6 s each).
package main

import (
	"machine"
	"strings"
	"time"
)

func main() {
	con := newConsole(openDisplay())
	editor := &lineEditor{con: con, keys: openKeyboard()}

	con.status("tiny-knx-go  cardputer", cyan)
	con.print("  ()-()", grey)
	con.print(`   \"/_`, grey)
	con.print("    '  )", grey)

	s := startSettings(con)
	s.show(con)

	if !s.complete() {
		con.status("tiny-knx-go  settings", yellow)
		con.print("set ssid, wifipass and gw, then save and reboot", yellow)
		con.print(settingsHelp, grey)
		con.input("")

		for {
			if line, ok := editor.poll(); ok && !s.configure(con, line) {
				con.print(settingsHelp, grey)
			}

			time.Sleep(10 * time.Millisecond)
		}
	}

	ip := connectWiFi(con, s)
	con.print("wifi: "+ip, green)
	con.input("")

	config := tunnelConfig(s)
	tunnel := dialGateway(con, ip, s.gateway, config)
	con.print(help, grey)

	ticker := time.NewTicker(10 * time.Millisecond)

	for {
		select {
		case ind, ok := <-tunnel.Inbound():
			if !ok {
				con.print("tunnel closed", red)
				tunnel = dialGateway(con, ip, s.gateway, config)
				break
			}

			line, c := describe(ind)
			con.print(line, c)

		case <-ticker.C:
			if line, ok := editor.poll(); ok && !s.configure(con, line) {
				execute(con, tunnel, line)
			}
		}
	}
}

// A lineEditor collects keys typed on the keyboard, or on the USB serial console, into a line.
type lineEditor struct {
	con   *console
	keys  *keyboard
	input []byte
}

// poll reads the keyboard and the serial console and returns a line when Enter was pressed.
func (e *lineEditor) poll() (line string, done bool) {
	for !done && machine.Serial.Buffered() > 0 {
		c, err := machine.Serial.ReadByte()
		if err != nil {
			break
		}

		switch c {
		case '\r', '\n':
			if len(e.input) > 0 {
				line, done = e.key(keyEnter)
			}
		case 0x08, 0x7F:
			e.key(keyBackspace)
		default:
			e.key(c)
		}
	}

	e.keys.poll(func(key byte) {
		if !done {
			line, done = e.key(key)
		}
	})

	return line, done
}

// key edits the line and returns it when key is Enter.
func (e *lineEditor) key(key byte) (line string, done bool) {
	switch key {
	case keyEnter:
		line, done = string(e.input), true
		e.input = e.input[:0]
		e.con.print("> "+masked(line), white)
	case keyBackspace:
		if len(e.input) > 0 {
			e.input = e.input[:len(e.input)-1]
		}
	default:
		if key >= ' ' && key < 0x7F {
			e.input = append(e.input, key)
		}
	}

	e.con.input(string(e.input))

	return line, done
}

// masked hides the password of a settings command.
func masked(line string) string {
	command, arg, _ := strings.Cut(line, " ")

	switch command {
	case "wifipass", "knxpass", "auth":
		if arg != "" && arg != "-" {
			return command + " ***"
		}
	}

	return line
}
