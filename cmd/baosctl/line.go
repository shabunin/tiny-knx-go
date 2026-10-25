//go:build linux && !tinygo

package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

// maxHistory is how many lines the history file keeps.
const maxHistory = 500

// A lineEditor reads lines from a terminal in raw mode, with history (up, down), cursor movement
// (left, right, home, end, Ctrl-A, Ctrl-E), deleting (backspace, delete, Ctrl-U, Ctrl-K, Ctrl-W)
// and completion of the first word (tab). Print writes above the line being edited.
type lineEditor struct {
	prompt   string
	commands []string
	file     string
	in       *bufio.Reader
	saved    syscall.Termios

	mu      sync.Mutex // guards the terminal and buf, pos
	buf     []rune
	pos     int
	history []string
	once    sync.Once
}

var errInterrupted = errors.New("interrupted")

func ioctl(request uintptr, t *syscall.Termios) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, os.Stdin.Fd(), request, uintptr(unsafe.Pointer(t)))
	if errno != 0 {
		return errno
	}

	return nil
}

// newLineEditor puts the terminal on standard input in raw mode and reads the history file.
func newLineEditor(prompt string, commands []string, historyFile string) (*lineEditor, error) {
	e := &lineEditor{prompt: prompt, commands: commands, file: historyFile, in: bufio.NewReader(os.Stdin)}

	if err := ioctl(syscall.TCGETS, &e.saved); err != nil {
		return nil, err
	}

	raw := e.saved
	raw.Lflag &^= syscall.ICANON | syscall.ECHO | syscall.ISIG | syscall.IEXTEN
	raw.Iflag &^= syscall.IXON | syscall.ICRNL
	raw.Cc[syscall.VMIN], raw.Cc[syscall.VTIME] = 1, 0

	if err := ioctl(syscall.TCSETS, &raw); err != nil {
		return nil, err
	}

	if data, err := os.ReadFile(historyFile); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if line != "" {
				e.history = append(e.history, line)
			}
		}
	}

	// Signals no longer come from the keyboard, but kill and hangup must not leave the terminal raw.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGHUP)

	go func() {
		<-signals
		e.Close()
		os.Exit(1)
	}()

	return e, nil
}

// Close restores the terminal and writes the history file.
func (e *lineEditor) Close() {
	e.once.Do(func() {
		e.mu.Lock()
		defer e.mu.Unlock()

		ioctl(syscall.TCSETS, &e.saved)

		history := e.history
		if len(history) > maxHistory {
			history = history[len(history)-maxHistory:]
		}

		if e.file != "" {
			os.MkdirAll(dirOf(e.file), 0o755)
			os.WriteFile(e.file, []byte(strings.Join(history, "\n")+"\n"), 0o644)
		}
	})
}

func dirOf(path string) string {
	if i := strings.LastIndexByte(path, '/'); i > 0 {
		return path[:i]
	}

	return "."
}

// redraw writes the prompt and the line and puts the cursor in place. It is called with mu held.
func (e *lineEditor) redraw() {
	s := "\r\033[K" + e.prompt + string(e.buf)
	if back := len(e.buf) - e.pos; back > 0 {
		s += fmt.Sprintf("\033[%dD", back)
	}

	os.Stdout.WriteString(s)
}

// Print writes a line above the line being edited.
func (e *lineEditor) Print(line string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	os.Stdout.WriteString("\r\033[K" + strings.ReplaceAll(line, "\n", "\r\n") + "\r\n")
	e.redraw()
}

// ReadLine reads a line. It returns io.EOF for Ctrl-D on an empty line, and errInterrupted for
// Ctrl-C on an empty line; Ctrl-C clears a line that is not empty.
func (e *lineEditor) ReadLine() (string, error) {
	e.mu.Lock()
	e.buf, e.pos = nil, 0
	e.redraw()
	e.mu.Unlock()

	index := len(e.history) // in the history; len(history) is the new line
	draft := ""

	recall := func(i int) {
		if i < 0 || i > len(e.history) {
			return
		}

		if index == len(e.history) {
			draft = string(e.buf)
		}

		index = i

		line := draft
		if i < len(e.history) {
			line = e.history[i]
		}

		e.buf = []rune(line)
		e.pos = len(e.buf)
	}

	for {
		r, _, err := e.in.ReadRune()
		if err != nil {
			return "", err
		}

		e.mu.Lock()

		switch r {
		case '\r', '\n':
			line := strings.TrimSpace(string(e.buf))
			os.Stdout.WriteString("\r\n")

			if line != "" && (len(e.history) == 0 || e.history[len(e.history)-1] != line) {
				e.history = append(e.history, line)
			}

			e.buf, e.pos = nil, 0
			e.mu.Unlock()

			return line, nil

		case 3: // Ctrl-C
			if len(e.buf) == 0 {
				os.Stdout.WriteString("\r\n")
				e.mu.Unlock()

				return "", errInterrupted
			}

			os.Stdout.WriteString("^C\r\n")
			e.buf, e.pos, index = nil, 0, len(e.history)

		case 4: // Ctrl-D
			if len(e.buf) == 0 {
				os.Stdout.WriteString("\r\n")
				e.mu.Unlock()

				return "", io.EOF
			}

			e.deleteAt(e.pos)

		case 127, 8: // backspace
			if e.pos > 0 {
				e.pos--
				e.deleteAt(e.pos)
			}

		case 1: // Ctrl-A
			e.pos = 0

		case 5: // Ctrl-E
			e.pos = len(e.buf)

		case 2: // Ctrl-B
			e.pos = max(e.pos-1, 0)

		case 6: // Ctrl-F
			e.pos = min(e.pos+1, len(e.buf))

		case 11: // Ctrl-K
			e.buf = e.buf[:e.pos]

		case 21: // Ctrl-U
			e.buf, e.pos = e.buf[e.pos:], 0

		case 23: // Ctrl-W
			start := e.pos
			for start > 0 && e.buf[start-1] == ' ' {
				start--
			}

			for start > 0 && e.buf[start-1] != ' ' {
				start--
			}

			e.buf = append(e.buf[:start], e.buf[e.pos:]...)
			e.pos = start

		case 12: // Ctrl-L
			os.Stdout.WriteString("\033[H\033[2J")

		case 16: // Ctrl-P
			recall(index - 1)

		case 14: // Ctrl-N
			recall(index + 1)

		case '\t':
			e.complete()

		case 27: // escape sequences of the keys
			e.mu.Unlock()
			key := e.readEscape()
			e.mu.Lock()

			switch key {
			case "A":
				recall(index - 1)
			case "B":
				recall(index + 1)
			case "C":
				e.pos = min(e.pos+1, len(e.buf))
			case "D":
				e.pos = max(e.pos-1, 0)
			case "H", "1~", "7~":
				e.pos = 0
			case "F", "4~", "8~":
				e.pos = len(e.buf)
			case "3~":
				e.deleteAt(e.pos)
			}

		default:
			if r >= ' ' {
				e.buf = append(e.buf[:e.pos], append([]rune{r}, e.buf[e.pos:]...)...)
				e.pos++
			}
		}

		e.redraw()
		e.mu.Unlock()
	}
}

func (e *lineEditor) deleteAt(i int) {
	if i < len(e.buf) {
		e.buf = append(e.buf[:i], e.buf[i+1:]...)
	}
}

// readEscape reads the rest of an escape sequence, ESC [ ... or ESC O ..., and returns what
// follows the bracket, e.g. "A" for up or "3~" for delete.
func (e *lineEditor) readEscape() string {
	r, _, err := e.in.ReadRune()
	if err != nil || (r != '[' && r != 'O') {
		return ""
	}

	var key []rune

	for {
		r, _, err := e.in.ReadRune()
		if err != nil {
			return ""
		}

		key = append(key, r)

		if (r >= 'A' && r <= 'Z') || r == '~' {
			return string(key)
		}

		if len(key) > 8 {
			return ""
		}
	}
}

// complete completes the command at the start of the line, or lists the commands it may be.
func (e *lineEditor) complete() {
	word := string(e.buf[:e.pos])
	if strings.ContainsRune(word, ' ') {
		return
	}

	var matches []string

	for _, c := range e.commands {
		if strings.HasPrefix(c, word) {
			matches = append(matches, c)
		}
	}

	switch len(matches) {
	case 0:
		return

	case 1:
		e.insert(matches[0][len(word):] + " ")

	default:
		prefix := matches[0]
		for _, m := range matches[1:] {
			for !strings.HasPrefix(m, prefix) {
				prefix = prefix[:len(prefix)-1]
			}
		}

		if len(prefix) > len(word) {
			e.insert(prefix[len(word):])
		} else {
			os.Stdout.WriteString("\r\033[K" + strings.Join(matches, "  ") + "\r\n")
		}
	}
}

func (e *lineEditor) insert(s string) {
	r := []rune(s)
	e.buf = append(e.buf[:e.pos], append(r, e.buf[e.pos:]...)...)
	e.pos += len(r)
}
