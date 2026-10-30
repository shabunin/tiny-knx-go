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
// sockets are connected).
//
//	tinygo flash -target=m5stamp-s3a -port /dev/ttyACM0 \
//	  -ldflags "-X main.ssid=SSID -X main.password=PASSWORD -X main.gateway=192.168.1.10" .
package main

import "time"

func main() {
	con := newConsole(openDisplay())
	keys := openKeyboard()

	con.status("tiny-knx-go  cardputer", cyan)
	con.print("  ()-()", grey)
	con.print(`   \"/_`, grey)
	con.print("    '  )", grey)

	if ssid == "" || gateway == "" {
		con.print("built without -X main.ssid and main.gateway", red)
		select {}
	}

	ip := connectWiFi(con)
	con.print("wifi: "+ip, green)

	var input []byte
	con.input("")

	tunnel := dialGateway(con, ip)
	con.print(help, grey)

	ticker := time.NewTicker(10 * time.Millisecond)

	for {
		select {
		case ind, ok := <-tunnel.Inbound():
			if !ok {
				con.print("tunnel closed", red)
				tunnel = dialGateway(con, ip)
				break
			}

			line, c := describe(ind)
			con.print(line, c)

		case <-ticker.C:
			keys.poll(func(key byte) {
				switch key {
				case keyEnter:
					con.print("> "+string(input), white)
					execute(con, tunnel, string(input))
					input = input[:0]
				case keyBackspace:
					if len(input) > 0 {
						input = input[:len(input)-1]
					}
				default:
					if key >= ' ' && key < 0x7F {
						input = append(input, key)
					}
				}

				con.input(string(input))
			})
		}
	}
}
