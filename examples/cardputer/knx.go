//go:build tinygo

package main

import (
	"fmt"
	"image/color"
	"strconv"
	"strings"
	"time"

	"tinygo.org/x/drivers/netdev"
	nl "tinygo.org/x/drivers/netlink"
	espnet "tinygo.org/x/espradio/netlink"

	"github.com/shabunin/tiny-knx-go/cemi"
	"github.com/shabunin/tiny-knx-go/dpt"
	"github.com/shabunin/tiny-knx-go/knxnet"
	"github.com/shabunin/tiny-knx-go/link/knxip"
)

// Optional settings at build time: -ldflags "-X main.ssid=... -X main.password=... -X
// main.gateway=host[:port]". With -X main.knxPassword=... the tunnel is opened in a KNX IP Secure
// session as main.knxUser (2 by default), checking the interface's device authentication code if
// main.knxAuth is set. They are saved to flash (see startSettings) and can be changed on the
// keyboard.
var (
	ssid     string
	password string
	gateway  string

	knxUser     = "2"
	knxPassword string
	knxAuth     string
)

var wifi espnet.Esplink

func connectWiFi(con *console, s *settings) string {
	netdev.UseNetdev(&wifi)

	for try := 0; ; try++ {
		con.status("wifi: connecting to "+s.ssid, yellow)

		err := wifi.NetConnect(&nl.ConnectParams{Ssid: s.ssid, Passphrase: s.wifiPassword})
		if err == nil {
			break
		}

		con.print("wifi: "+err.Error(), red)

		// The first attempt after a restart often ends in a handshake timeout (of the Wi-Fi
		// blob, after 12 s) and the next one succeeds at once.
		if try > 0 {
			time.Sleep(3 * time.Second)
		}
	}

	addr, _ := wifi.Addr()

	return addr.String()
}

// tunnelConfig opens a secure tunnel if the settings have a password key.
func tunnelConfig(s *settings) knxip.TunnelConfig {
	config := knxip.DefaultTunnelConfig

	if s.passwordKey != nil {
		config.Tunnel.Secure = &knxnet.TunnelSecure{User: s.user, Password: *s.passwordKey}
		if s.authKey != nil {
			config.Tunnel.Secure.DeviceAuth = *s.authKey
		}
	}

	return config
}

// dialGateway opens a tunnel, trying again until it succeeds.
func dialGateway(con *console, ip, gateway string, config knxip.TunnelConfig) *knxip.Tunnel {
	kind := "tunnel"
	if config.Tunnel.Secure != nil {
		kind = "secure tunnel"
	}

	for {
		con.status(ip+"  knx: "+gateway+"...", yellow)

		t, err := knxip.DialTunnel(gateway, config)
		if err == nil {
			con.status(ip+"  knx: "+t.Address().String()+" @ "+gateway, green)
			con.print(kind+" open, address "+t.Address().String(), green)

			return t
		}

		con.print("knx: "+err.Error(), red)
		time.Sleep(3 * time.Second)
	}
}

// describe shows a frame from the bus in one short line.
func describe(ind *cemi.LDataInd) (string, color.RGBA) {
	var dest string
	if ind.Control2.IsGroupAddr() {
		dest = cemi.GroupAddr(ind.Destination).String()
	} else {
		dest = cemi.IndividualAddr(ind.Destination).String()
	}

	app, ok := ind.Data.(*cemi.AppData)
	if !ok {
		return ind.Source.String() + " " + dest + " T_ctrl", grey
	}

	var name string
	c := grey

	switch app.Command {
	case cemi.GroupValueWrite:
		name, c = "W", green
	case cemi.GroupValueRead:
		name, c = "R", yellow
	case cemi.GroupValueResponse:
		name, c = "A", cyan
	default:
		return fmt.Sprintf("%s %s apci %03x", ind.Source, dest, uint16(app.Command)), grey
	}

	return fmt.Sprintf("%-8s %-8s %s %s", ind.Source, dest, name, value(app)), c
}

// value shows the data of a group service, with a guess at its meaning: KNX frames don't say
// their datapoint type.
func value(app *cemi.AppData) string {
	switch data := app.Data; {
	case app.Command == cemi.GroupValueRead:
		return ""
	case len(data) == 1 && data[0] <= 1:
		return [2]string{"off", "on"}[data[0]]
	case len(data) == 1:
		return fmt.Sprintf("%02x", data[0])
	case len(data) == 2:
		return fmt.Sprintf("%02x %d", data[1], data[1])
	case len(data) == 3:
		var f dpt.DPT_9001
		if f.Unpack(data) == nil {
			return fmt.Sprintf("%02x%02x %.2f", data[1], data[2], float32(f))
		}

		return fmt.Sprintf("%02x%02x", data[1], data[2])
	default:
		return fmt.Sprintf("% x", data[1:])
	}
}

const help = "w GA on|off|N|N.N|N%|xHEX, r GA, clear, set"

// execute runs a command typed on the keyboard.
func execute(con *console, t *knxip.Tunnel, text string) {
	args := strings.Fields(text)
	if len(args) == 0 {
		return
	}

	switch {
	case args[0] == "w" && len(args) == 3:
		data, err := parseValue(args[2])
		if err != nil {
			con.print(err.Error(), red)
			return
		}

		send(con, t, args[1], cemi.GroupValueWrite, data)

	case args[0] == "r" && len(args) == 2:
		send(con, t, args[1], cemi.GroupValueRead, nil)

	case args[0] == "clear":
		for range rows {
			con.print("", black)
		}

	default:
		con.print(help, grey)
	}
}

// parseValue turns a typed value into the data of a group write.
func parseValue(s string) ([]byte, error) {
	switch {
	case s == "on" || s == "1":
		return []byte{1}, nil
	case s == "off" || s == "0":
		return []byte{0}, nil
	case strings.HasPrefix(s, "x"):
		var data []byte

		for s = s[1:]; len(s) > 0; s = s[min(2, len(s)):] {
			b, err := strconv.ParseUint(s[:min(2, len(s))], 16, 8)
			if err != nil {
				return nil, fmt.Errorf("bad hex")
			}

			data = append(data, byte(b))
		}

		// Values up to 6 bits are packed into the APCI byte; longer ones follow it.
		if len(data) > 1 || len(data) == 1 && data[0] > 0x3F {
			data = append([]byte{0}, data...)
		}

		return data, nil
	case strings.HasSuffix(s, "%"):
		f, err := strconv.ParseFloat(s[:len(s)-1], 32)
		if err != nil {
			return nil, err
		}

		return dpt.DPT_5001(f).Pack(), nil
	case strings.Contains(s, "."):
		f, err := strconv.ParseFloat(s, 32)
		if err != nil {
			return nil, err
		}

		return dpt.DPT_9001(f).Pack(), nil
	}

	n, err := strconv.ParseUint(s, 10, 8)
	if err != nil {
		return nil, fmt.Errorf("bad value %q", s)
	}

	return []byte{0, byte(n)}, nil
}

func send(con *console, t *knxip.Tunnel, addr string, command cemi.APCI, data []byte) {
	ga, err := cemi.NewGroupAddrString(addr)
	if err != nil {
		con.print("bad group address "+addr, red)
		return
	}

	req := &cemi.LDataReq{LData: cemi.LData{
		Control1:    cemi.Control1Request(cemi.PrioLow),
		Control2:    cemi.Control2GroupAddr | cemi.Control2Hops(6),
		Destination: uint16(ga),
		Data:        &cemi.AppData{Command: command, Data: data},
	}}

	if err := t.Send(req); err != nil {
		con.print("send: "+err.Error(), red)
		return
	}

	// The tunnel does not echo our own frames, so show them here.
	ind := cemi.LDataInd{LData: req.LData}
	ind.Source = t.Address()
	line, c := describe(&ind)
	con.print(line, c)
}
