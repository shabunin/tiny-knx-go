//go:build tinygo

package main

import "machine"

// The keyboard of the original Cardputer is a matrix: a 74HC138 selects one of 8 lines with the
// address pins, and the 7 input pins read which keys of the line are pressed (low). Every line
// covers every other column of half the rows; see M5Cardputer's IOMatrixKeyboardReader.
var (
	keyAddress = [3]machine.Pin{machine.GPIO8, machine.GPIO9, machine.GPIO11}
	keyInputs  = [7]machine.Pin{machine.GPIO13, machine.GPIO15, machine.GPIO3, machine.GPIO4, machine.GPIO5, machine.GPIO6, machine.GPIO7}
)

// Special keys.
const (
	keyNone = 0
	keyFn   = 0x80 + iota
	keyShift
	keyCtrl
	keyOpt
	keyAlt
	keyEnter
	keyBackspace
	keyDelete
	keyTab
	keyEscape
	keyUp
	keyDown
	keyLeft
	keyRight
)

// keyMap has the legends of the 4×14 keys: plain, with shift and with fn.
var keyMap = [4][14][3]byte{
	{{'`', '~', keyEscape}, {'1', '!'}, {'2', '@'}, {'3', '#'}, {'4', '$'}, {'5', '%'}, {'6', '^'},
		{'7', '&'}, {'8', '*'}, {'9', '('}, {'0', ')'}, {'-', '_'}, {'=', '+'}, {keyBackspace, keyBackspace, keyDelete}},
	{{keyTab, keyTab}, {'q', 'Q'}, {'w', 'W'}, {'e', 'E'}, {'r', 'R'}, {'t', 'T'}, {'y', 'Y'},
		{'u', 'U'}, {'i', 'I'}, {'o', 'O'}, {'p', 'P'}, {'[', '{'}, {']', '}'}, {'\\', '|'}},
	{{keyFn, keyFn, keyFn}, {keyShift, keyShift}, {'a', 'A'}, {'s', 'S'}, {'d', 'D'}, {'f', 'F'}, {'g', 'G'},
		{'h', 'H'}, {'j', 'J'}, {'k', 'K'}, {'l', 'L'}, {';', ':', keyUp}, {'\'', '"'}, {keyEnter, keyEnter}},
	{{keyCtrl, keyCtrl}, {keyOpt, keyOpt}, {keyAlt, keyAlt}, {'z', 'Z'}, {'x', 'X'}, {'c', 'C'}, {'v', 'V'},
		{'b', 'B'}, {'n', 'N'}, {'m', 'M'}, {',', '<', keyLeft}, {'.', '>', keyDown}, {'/', '?', keyRight}, {' ', ' '}},
}

type keyboard struct {
	pressed [4][14]bool
}

func openKeyboard() *keyboard {
	for _, p := range keyAddress {
		p.Configure(machine.PinConfig{Mode: machine.PinOutput})
		p.Low()
	}

	for _, p := range keyInputs {
		p.Configure(machine.PinConfig{Mode: machine.PinInputPullup})
	}

	return &keyboard{}
}

// scan reads the matrix.
func (k *keyboard) scan() (keys [4][14]bool) {
	for line := range 8 {
		for i, p := range keyAddress {
			p.Set(line&(1<<i) != 0)
		}

		for j, p := range keyInputs {
			if p.Get() {
				continue
			}

			x := 2*j + 1
			if line > 3 {
				x = 2 * j
			}

			keys[3-line%4][x] = true
		}
	}

	return keys
}

// poll returns the keys that went down since the last call, with shift and fn applied.
func (k *keyboard) poll(f func(key byte)) {
	keys := k.scan()
	shift, fn := keys[2][1], keys[2][0]

	for y := range keys {
		for x, down := range keys[y] {
			if down && !k.pressed[y][x] {
				legend := keyMap[y][x]
				key := legend[0]

				switch {
				case fn && legend[2] != keyNone:
					key = legend[2]
				case shift:
					key = legend[1]
				}

				if key != keyShift && key != keyFn {
					f(key)
				}
			}
		}
	}

	k.pressed = keys
}
