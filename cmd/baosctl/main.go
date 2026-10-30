//go:build linux && !tinygo

// Command baosctl is a REPL for the datapoints of a BAOS module that knxgw serves as JSON
// (-o json:PATH), like dobaos.tool: it lists the datapoints, reads and sets their values, names
// them and prints the values that change, in colors chosen with watch.
//
//	$ baosctl
//	baos> description *
//	baos> get 1 2 3
//	baos> set 1: true
//	baos> set [2: 21.5, lights: false]
//	baos> watch 2: red
//
// With arguments it runs them as one command and exits: baosctl get 1 2. In a terminal it has
// history (kept in ~/.config/baosctl/history), arrow keys and completion of commands (tab).
package main

import (
	"bufio"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shabunin/tiny-knx-go/internal/sdk"
)

const help = `Datapoints:
  description ( * | 1 2 3 | [1, 2, 3] )
  get ( * | 1 2 3 | lights )          values from the module
  stored ( * | 1 2 3 )                values as last known
  read 1 2 3                          send read requests; responses come as values
  set 1: true                         set and send to the bus
  set [lights: true, 2: 21.5]
  set 9: {day: 1, hour: 12, minutes: 30, seconds: 0}
  put 1: true                         set without sending
  raw 2: 0xffddcc | 3: [1, 2] | 3: "AQ=="
Names:
  name 1: lights | name 1: ?          name a datapoint, or show its name
  unname lights
Monitoring:
  watch 1: red | [1: red, 2: green, 3: underline, 4: hide]
  unwatch 1 2 3
Service:
  serveritems
  progmode ( ? | true | false | 1 | 0 )
  reset, version, help, exit
`

var (
	client      *sdk.Client
	interactive bool
	printJSON   bool
	config      = toolConfig{Watch: map[string]string{}}
	configPath  string
)

type toolConfig struct {
	Watch map[string]string `json:"watch"`
}

func main() {
	socket := flag.String("s", envOr("BAOSCTL_SOCKET", "/tmp/knxgw.json"), "unix socket of knxgw -o json (env BAOSCTL_SOCKET)")
	flag.BoolVar(&printJSON, "json", false, "with a command: print the payload of the response as JSON")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: baosctl [options] [COMMAND]\n\n%s\nWith a command, also: monitor (print values as they change).\n\nOptions:\n", help)
		flag.PrintDefaults()
	}
	flag.Parse()

	loadConfig()

	var err error
	if client, err = sdk.Dial(*socket); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer client.Close()

	if flag.NArg() > 0 {
		if err := execute(strings.Join(flag.Args(), " ")); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}

		return
	}

	repl(*socket)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return def
}

// The configuration keeps the colors of watch, like dobaos.tool.
func loadConfig() {
	dir, err := os.UserConfigDir()
	if err != nil {
		return
	}

	configPath = filepath.Join(dir, "baosctl", "tool.json")

	if data, err := os.ReadFile(configPath); err == nil {
		json.Unmarshal(data, &config)
	}

	if config.Watch == nil {
		config.Watch = map[string]string{}
	}
}

func saveConfig() {
	if configPath == "" {
		return
	}

	os.MkdirAll(filepath.Dir(configPath), 0o755)
	data, _ := json.MarshalIndent(config, "", "  ")
	os.WriteFile(configPath, append(data, '\n'), 0o644)
}

const prompt = "baos> "

var commands = []string{
	"description", "get", "stored", "read", "set", "put", "raw", "serveritems", "progmode",
	"name", "unname", "watch", "unwatch", "reset", "version", "help", "exit",
}

var (
	outMu    sync.Mutex
	terminal bool
	editor   *lineEditor
)

// out prints a line, in the REPL above the line being edited.
func out(line string) {
	if editor != nil {
		editor.Print(line)
		return
	}

	outMu.Lock()
	defer outMu.Unlock()

	fmt.Println(line)
}

func repl(socket string) {
	interactive = true

	info, _ := os.Stdin.Stat()
	terminal = info != nil && info.Mode()&os.ModeCharDevice != 0

	fmt.Println("hello, friend")
	fmt.Println("connected to", socket)

	// Without a terminal, e.g. with commands from a file, lines are read as they are.
	read := func() (string, error) { return "", io.EOF }

	if terminal {
		history := ""
		if configPath != "" {
			history = filepath.Join(filepath.Dir(configPath), "history")
		}

		var err error
		if editor, err = newLineEditor(prompt, commands, history); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer editor.Close()

		read = editor.ReadLine
	} else {
		scanner := bufio.NewScanner(os.Stdin)
		read = func() (string, error) {
			if scanner.Scan() {
				return scanner.Text(), nil
			}

			return "", io.EOF
		}
	}

	go func() {
		for m := range client.Broadcasts() {
			printBroadcast(m)
		}

		select {
		case <-done:
		default:
			out("the connection to knxgw closed")

			if editor != nil {
				editor.Close()
			}

			os.Exit(1)
		}
	}()

	for {
		line, err := read()
		if err != nil {
			break
		}

		line = strings.TrimSpace(line)
		if line == "exit" || line == "quit" {
			break
		}

		if line != "" {
			if err := execute(line); err != nil {
				out(err.Error())
			}
		}
	}

	close(done)
}

var done = make(chan struct{})

func execute(line string) error {
	cmd, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
	rest = strings.TrimSpace(rest)

	switch cmd {
	case "description", "descr":
		return request("get description", ids(rest), printDescriptions)

	case "get":
		return request("get value", ids(rest), printValues)

	case "stored":
		return request("get stored", ids(rest), printValues)

	case "read":
		if rest == "" {
			return errors.New("read which datapoints?")
		}

		return request("read value", ids(rest), nil)

	case "set", "put", "raw":
		pairs, err := parsePairs(rest)
		if err != nil {
			return err
		}

		var payload []map[string]any

		for _, p := range pairs {
			m := map[string]any{"id": p.id}

			if cmd == "raw" {
				raw, err := rawValue(p.value)
				if err != nil {
					return err
				}

				m["raw"] = raw
			} else {
				value, err := jsonValue(p.value)
				if err != nil {
					return err
				}

				m["value"] = value
			}

			payload = append(payload, m)
		}

		method := "set value"
		if cmd == "put" {
			method = "put value"
		}

		// In the REPL the values come back as a broadcast.
		var print func(json.RawMessage) error
		if !interactive {
			print = printValues
		}

		return request(method, payload, print)

	case "serveritems", "items":
		return request("get server items", nil, printItems)

	case "progmode":
		if rest != "" && rest != "?" {
			on := rest == "true" || rest == "1" || rest == "on"
			if err := request("set programming mode", on, nil); err != nil {
				return err
			}
		}

		return request("get programming mode", nil, func(p json.RawMessage) error {
			out("BAOS module in programming mode: " + string(p))
			return nil
		})

	case "name":
		pairs, err := parsePairs(rest)
		if err != nil {
			return err
		}

		for _, p := range pairs {
			if p.value == "?" {
				err = request("get name", p.id, func(res json.RawMessage) error {
					var name *string
					json.Unmarshal(res, &name)

					if name == nil {
						out(fmt.Sprintf("%v has no name", p.id))
					} else {
						out(fmt.Sprintf("%v: %s", p.id, *name))
					}

					return nil
				})
			} else {
				name, _ := jsonValue(p.value)
				err = request("set name", map[string]any{"id": p.id, "name": fmt.Sprint(name)}, func(json.RawMessage) error {
					out(fmt.Sprintf("%v: %v", p.id, name))
					return nil
				})
			}

			if err != nil {
				return err
			}
		}

		return nil

	case "unname":
		return request("remove name", ids(rest), func(json.RawMessage) error {
			out("ok")
			return nil
		})

	case "watch":
		if rest == "" {
			for id, color := range config.Watch {
				out(fmt.Sprintf("datapoint %s: %s", id, color))
			}

			return nil
		}

		pairs, err := parsePairs(rest)
		if err != nil {
			return err
		}

		for _, p := range pairs {
			color, _ := jsonValue(p.value)
			config.Watch[fmt.Sprint(p.id)] = fmt.Sprint(color)
			out(fmt.Sprintf("datapoint %v value is now in %v", p.id, color))
		}

		saveConfig()

		return nil

	case "unwatch":
		list, _ := ids(rest).([]any)
		for _, id := range list {
			delete(config.Watch, fmt.Sprint(id))
			out(fmt.Sprintf("datapoint %v value is now in default color", id))
		}

		saveConfig()

		return nil

	case "reset":
		return request("reset", nil, func(p json.RawMessage) error {
			out("reset request result: " + string(p))
			return nil
		})

	case "version":
		return request("version", nil, func(p json.RawMessage) error {
			out("server version is " + string(p))
			return nil
		})

	case "monitor":
		if interactive {
			return errors.New("the REPL prints the values anyway")
		}

		for m := range client.Broadcasts() {
			printBroadcast(m)
		}

		return errors.New("the connection to knxgw closed")

	case "help":
		out(strings.TrimRight(help, "\n"))
		return nil
	}

	return fmt.Errorf("unknown command %q, try help", cmd)
}

func request(method string, payload any, print func(json.RawMessage) error) error {
	res, err := client.Call(method, payload)
	if err != nil {
		return err
	}

	if printJSON && !interactive {
		fmt.Println(string(res))
		return nil
	}

	if print == nil {
		return nil
	}

	return print(res)
}

// ids returns the datapoints of "1 2 3", "[1, 2, 3]", names, or "*" or nothing for all.
func ids(s string) any {
	fields := strings.Fields(strings.NewReplacer("[", " ", "]", " ", ",", " ").Replace(s))

	var list []any

	for _, f := range fields {
		if f == "*" {
			return nil
		}

		list = append(list, parseID(f))
	}

	if len(list) == 0 {
		return nil
	}

	return list
}

func parseID(s string) any {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}

	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}

	return s
}

type pair struct {
	id    any
	value string
}

// parsePairs scans "1: true", "[1: true, lights: false]" or "2: {day: 1, hour: 12}": an ID, a
// colon (or =), and a value, which runs to the end of its quotes or brackets, or to a space.
func parsePairs(s string) ([]pair, error) {
	var out []pair

	i := 0
	skip := func(chars string) {
		for i < len(s) && strings.IndexByte(chars, s[i]) >= 0 {
			i++
		}
	}

	for {
		skip(" \t,[]")
		if i == len(s) {
			break
		}

		start := i
		if s[i] == '"' || s[i] == '\'' {
			end := strings.IndexByte(s[i+1:], s[i])
			if end < 0 {
				return nil, errors.New("unterminated quote")
			}

			i += end + 2
		} else {
			for i < len(s) && strings.IndexByte(":= \t", s[i]) < 0 {
				i++
			}
		}

		id := s[start:i]

		skip(" \t")
		if i == len(s) || (s[i] != ':' && s[i] != '=') {
			return nil, fmt.Errorf("expected %s: VALUE", id)
		}

		i++
		skip(" \t")

		start = i
		end, err := valueEnd(s, i)
		if err != nil {
			return nil, err
		}

		i = end
		if start == end {
			return nil, fmt.Errorf("no value for %s", id)
		}

		out = append(out, pair{parseID(id), s[start:end]})
	}

	if len(out) == 0 {
		return nil, errors.New("expected ID: VALUE")
	}

	return out, nil
}

// valueEnd returns where the value at i ends.
func valueEnd(s string, i int) (int, error) {
	depth, quote := 0, byte(0)

	for ; i < len(s); i++ {
		c := s[i]

		switch {
		case quote != 0:
			if c == quote {
				quote = 0
				if depth == 0 {
					return i + 1, nil
				}
			}

		case c == '"' || c == '\'':
			quote = c

		case c == '{' || c == '[':
			depth++

		case c == '}' || c == ']':
			if depth == 0 {
				return i, nil // the end of a list of pairs
			}

			depth--
			if depth == 0 {
				return i + 1, nil
			}

		case depth == 0 && (c == ' ' || c == '\t' || c == ','):
			return i, nil
		}
	}

	if depth != 0 || quote != 0 {
		return 0, errors.New("unbalanced brackets or quotes")
	}

	return i, nil
}

var (
	bareKey      = regexp.MustCompile(`([{,]\s*)([A-Za-z_][A-Za-z0-9_]*)\s*:`)
	singleQuoted = regexp.MustCompile(`'([^']*)'`)
)

// jsonValue parses a value: JSON, also with bare keys and single quotes ({day: 1, hour: 'x'}),
// or else a word, which is a string.
func jsonValue(s string) (any, error) {
	text := singleQuoted.ReplaceAllString(s, `"$1"`)
	if strings.HasPrefix(text, "{") {
		text = bareKey.ReplaceAllString(text, `$1"$2":`)
	}

	var v any
	if err := json.Unmarshal([]byte(text), &v); err == nil {
		return v, nil
	}

	if strings.ContainsAny(s, "{}[]\"") {
		return nil, fmt.Errorf("bad value %s", s)
	}

	return s, nil
}

// rawValue parses a raw value, 0xHEX, [1, 2, 3] or a base64 string, into base64.
func rawValue(s string) (string, error) {
	switch {
	case strings.HasPrefix(s, "0x"):
		data, err := hex.DecodeString(s[2:])
		if err != nil {
			return "", fmt.Errorf("bad hex %s", s)
		}

		return base64.StdEncoding.EncodeToString(data), nil

	case strings.HasPrefix(s, "["):
		var list []byte
		var ints []int

		if err := json.Unmarshal([]byte(s), &ints); err != nil {
			return "", fmt.Errorf("bad bytes %s", s)
		}

		for _, n := range ints {
			if n < 0 || n > 255 {
				return "", fmt.Errorf("bad byte %d", n)
			}

			list = append(list, byte(n))
		}

		return base64.StdEncoding.EncodeToString(list), nil
	}

	text := strings.Trim(s, `"'`)
	if _, err := base64.StdEncoding.DecodeString(text); err != nil {
		return "", fmt.Errorf("bad base64 %s", s)
	}

	return text, nil
}

var colors = map[string]string{
	"black": "30", "red": "31", "green": "32", "yellow": "33", "blue": "34", "magenta": "35",
	"cyan": "36", "white": "37", "gray": "90", "grey": "90", "brightRed": "91", "brightGreen": "92",
	"brightYellow": "93", "brightBlue": "94", "brightMagenta": "95", "brightCyan": "96",
	"brightWhite": "97", "bold": "1", "dim": "2", "italic": "3", "underline": "4", "inverse": "7",
	"strikethrough": "9",
}

func colorOf(v value) string {
	if color, ok := config.Watch[strconv.Itoa(int(v.ID))]; ok {
		return color
	}

	return config.Watch[v.Name]
}

func paint(s, color string) string {
	if code, ok := colors[color]; ok && terminal {
		return "\033[" + code + "m" + s + "\033[0m"
	}

	return s
}

type value struct {
	ID    uint16          `json:"id"`
	Name  string          `json:"name"`
	Value json.RawMessage `json:"value"`
	Raw   []byte          `json:"raw"`
	Valid *bool           `json:"valid"`
}

func bytesList(data []byte) string {
	parts := make([]string, len(data))
	for i, b := range data {
		parts[i] = strconv.Itoa(int(b))
	}

	return strings.Join(parts, ",")
}

func formatValue(v value) string {
	id := strconv.Itoa(int(v.ID))
	if v.Name != "" {
		id += " <" + v.Name + ">"
	}

	text := string(v.Value)
	if text == "" {
		text = "-"
	}

	s := fmt.Sprintf("%s,    id: %s, value: %s, raw: [%s] [0x%x]", time.Now().Format("15:04:05.000"), id, text, bytesList(v.Raw), v.Raw)
	if v.Valid != nil && !*v.Valid {
		s += ", not from the bus"
	}

	return paint(s, colorOf(v))
}

func printValues(p json.RawMessage) error {
	var list []value
	if err := json.Unmarshal(p, &list); err != nil {
		var one value
		if err := json.Unmarshal(p, &one); err != nil {
			return err
		}

		list = []value{one}
	}

	for _, v := range list {
		out(formatValue(v))
	}

	return nil
}

func printDescriptions(p json.RawMessage) error {
	var list []sdk.Description
	if err := json.Unmarshal(p, &list); err != nil {
		return err
	}

	for _, d := range list {
		flags := []byte("CRWTU")
		for i, on := range []bool{d.Communication, d.Read, d.Write, d.Transmit, d.Update} {
			if !on {
				flags[i] = '-'
			}
		}

		name := d.Name
		if name == "" {
			name = "---"
		}

		typ := "unknown"
		if d.Type != 0 {
			typ = fmt.Sprintf("dpt%d", d.Type)
		}

		prio := strconv.Itoa(d.Priority)
		if d.Priority >= 0 && d.Priority < len(priorities) {
			prio = priorities[d.Priority]
		}

		out(fmt.Sprintf("#%d <%s>, %s, %d bits, prio: %s, flags: [%s]", d.ID, name, typ, d.Size, prio, flags))
	}

	return nil
}

// priorities are the names of the priorities that descriptions give as numbers.
var priorities = [...]string{"system", "high", "alarm", "low"}

var itemNames = map[uint16]string{
	1: "hardware type", 2: "hardware version", 3: "firmware version", 4: "manufacturer",
	5: "application vendor", 6: "application", 7: "application version", 8: "serial number",
	9: "ms since reset", 10: "bus connected", 11: "max buffer size", 12: "description length",
	13: "baud rate", 14: "buffer size", 15: "programming mode", 16: "protocol version",
	17: "indications",
}

func printItems(p json.RawMessage) error {
	var list []sdk.Item
	if err := json.Unmarshal(p, &list); err != nil {
		return err
	}

	for _, item := range list {
		out(fmt.Sprintf("server item %d <%s>, value: [%s] [0x%x]", item.ID, itemNames[item.ID], bytesList(item.Raw), item.Raw))
	}

	return nil
}

func printBroadcast(m sdk.Message) {
	switch m.Method {
	case "datapoint value":
		var list []value
		json.Unmarshal(m.Payload, &list)

		for _, v := range list {
			if color := colorOf(v); color == "hide" || color == "hidden" {
				continue
			}

			out(formatValue(v))
		}

	case "server item":
		printItems(m.Payload)

	default:
		out(m.Method + " " + string(m.Payload))
	}
}
