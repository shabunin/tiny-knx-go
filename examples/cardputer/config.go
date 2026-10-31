//go:build tinygo

package main

import (
	"crypto/sha256"
	"device/esp"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"machine"
	"strconv"
	"strings"

	"github.com/shabunin/tiny-knx-go/secure"
)

// settings are kept in the last sector of the flash data area, which starts after the program:
// flashing a larger program does not reach them. The KNX passwords are not kept, only the keys
// derived from them, which take seconds to compute.
type settings struct {
	ssid, wifiPassword, gateway string

	user        uint8
	passwordKey *[16]byte // nil: a plain tunnel
	authKey     *[16]byte // nil: the device authentication code is not checked

	seed [8]byte // of the build-time settings that were saved last
}

const settingsMagic = "TKC1"

var errNoSettings = errors.New("no settings in flash")

func settingsOffset() int64 {
	return machine.Flash.Size() - machine.Flash.EraseBlockSize()
}

func (s *settings) marshal() []byte {
	b := []byte(settingsMagic)

	for _, str := range []string{s.ssid, s.wifiPassword, s.gateway} {
		b = append(b, byte(len(str)))
		b = append(b, str...)
	}

	b = append(b, s.user)

	for _, key := range []*[16]byte{s.passwordKey, s.authKey} {
		if key == nil {
			b = append(b, 0)
		} else {
			b = append(b, 1)
			b = append(b, key[:]...)
		}
	}

	b = append(b, s.seed[:]...)

	return binary.LittleEndian.AppendUint32(b, crc32.ChecksumIEEE(b))
}

func unmarshalSettings(b []byte) (*settings, error) {
	if len(b) < 8 || string(b[:4]) != settingsMagic {
		return nil, errNoSettings
	}

	s := new(settings)
	p := b[4:]

	str := func() string {
		if len(p) < 1 || len(p) < 1+int(p[0]) {
			p = nil
			return ""
		}

		v := string(p[1 : 1+p[0]])
		p = p[1+p[0]:]

		return v
	}

	s.ssid, s.wifiPassword, s.gateway = str(), str(), str()

	key := func() *[16]byte {
		if len(p) < 1 || p[0] == 1 && len(p) < 17 {
			p = nil
			return nil
		}

		if p[0] == 0 {
			p = p[1:]
			return nil
		}

		k := new([16]byte)
		copy(k[:], p[1:17])
		p = p[17:]

		return k
	}

	if len(p) < 1 {
		return nil, errNoSettings
	}

	s.user, p = p[0], p[1:]
	s.passwordKey, s.authKey = key(), key()

	if len(p) < len(s.seed)+4 {
		return nil, errNoSettings
	}

	copy(s.seed[:], p)
	p = p[len(s.seed):]

	if n := len(b) - len(p); binary.LittleEndian.Uint32(p) != crc32.ChecksumIEEE(b[:n]) {
		return nil, errors.New("settings in flash are damaged")
	}

	return s, nil
}

func loadSettings() (*settings, error) {
	b := make([]byte, 256)
	if _, err := machine.Flash.ReadAt(b, settingsOffset()); err != nil {
		return nil, err
	}

	return unmarshalSettings(b)
}

func (s *settings) save() error {
	off := settingsOffset()
	size := machine.Flash.EraseBlockSize()

	if err := machine.Flash.EraseBlocks(off/size, 1); err != nil {
		return err
	}

	_, err := machine.Flash.WriteAt(s.marshal(), off)

	return err
}

// buildSeed identifies the settings given at build time; zero if there are none.
func buildSeed() (seed [8]byte) {
	if ssid == "" {
		return seed
	}

	sum := sha256.Sum256([]byte(strings.Join([]string{ssid, password, gateway, knxUser, knxPassword, knxAuth}, "\x00")))
	copy(seed[:], sum[:])

	return seed
}

// startSettings returns the settings in flash. Settings given at build time replace them once,
// when they differ from those saved last, so that a new build takes effect but changes made
// on the keyboard survive a restart.
func startSettings(con *console) *settings {
	s, err := loadSettings()
	seed := buildSeed()

	switch {
	case seed != [8]byte{} && (err != nil || s.seed != seed):
		s = &settings{ssid: ssid, wifiPassword: password, gateway: gateway, user: 2, seed: seed}

		if u, err := strconv.ParseUint(knxUser, 10, 8); err == nil && u >= 2 {
			s.user = uint8(u)
		}

		s.setKey(con, &s.passwordKey, knxPassword, secure.UserPasswordKey)
		s.setKey(con, &s.authKey, knxAuth, secure.DeviceAuthKey)

		if err := s.save(); err != nil {
			con.print("saving the settings: "+err.Error(), red)
		} else {
			con.print("settings of the build saved to flash", green)
		}

	case err != nil:
		con.print(err.Error(), yellow)
		s = &settings{user: 2}

	default:
		con.print("settings from flash", green)
	}

	return s
}

// setKey derives a key from a password, or clears it for an empty one or "-".
func (s *settings) setKey(con *console, key **[16]byte, password string, derive func(string) [16]byte) {
	if password == "" || password == "-" {
		*key = nil
		return
	}

	con.status("deriving a key...", yellow)

	k := derive(password)
	*key = &k
}

func (s *settings) complete() bool {
	return s.ssid != "" && s.gateway != ""
}

func (s *settings) show(con *console) {
	hidden := func(set bool) string {
		if set {
			return "set"
		}

		return "-"
	}

	con.print("ssid "+s.ssid+", wifipass "+hidden(s.wifiPassword != ""), white)
	con.print("gw "+s.gateway+", user "+strconv.Itoa(int(s.user)), white)
	con.print("knxpass "+hidden(s.passwordKey != nil)+", auth "+hidden(s.authKey != nil), white)
}

const settingsHelp = "ssid NAME, wifipass PW, gw HOST[:PORT], user N, knxpass PW|-, auth CODE|-, show, save, reboot"

// configure runs a settings command and reports whether it was one.
func (s *settings) configure(con *console, line string) bool {
	command, arg, _ := strings.Cut(strings.TrimSpace(line), " ")

	switch command {
	case "ssid":
		s.ssid = arg
	case "wifipass":
		s.wifiPassword = arg
	case "gw":
		s.gateway = arg
	case "user":
		u, err := strconv.ParseUint(arg, 10, 8)
		if err != nil || u < 2 {
			con.print("user: 2 or more", red)
			return true
		}

		s.user = uint8(u)
	case "knxpass":
		s.setKey(con, &s.passwordKey, arg, secure.UserPasswordKey)
	case "auth":
		s.setKey(con, &s.authKey, arg, secure.DeviceAuthKey)
	case "show":
		s.show(con)
		return true
	case "save":
		if err := s.save(); err != nil {
			con.print("save: "+err.Error(), red)
		} else {
			con.print("saved; reboot to apply", green)
		}

		return true
	case "reboot":
		esp.RTC_CNTL.SetOPTIONS0_SW_SYS_RST(1)
		select {}
	case "set", "help":
		con.print(settingsHelp, grey)
		return true
	default:
		return false
	}

	con.print("ok, not saved yet", grey)

	return true
}
