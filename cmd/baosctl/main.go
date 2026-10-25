//go:build linux && !tinygo

// Command baosctl talks to the datapoints of a BAOS module that knxgw serves as JSON (-o json:PATH),
// like dobaos-tool: it lists the datapoints, reads and sets their values, and prints the values that
// change. Without a command it reads commands from standard input and prints the broadcasts.
//
//	baosctl description            # all datapoints that ETS configured
//	baosctl get 1-3 room_temp      # values from the module, by ID, range or name
//	baosctl set 2=true 8=21.5      # set and send to the bus
//	baosctl set 9='{"day":1,"hour":12,"minutes":0,"seconds":0}'
//	baosctl watch                  # values as they change
package main

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/shabunin/tiny-knx-go/internal/sdk"
)

const help = `Commands:
  description [IDS]       datapoints (all without IDS); also descr, d
  get [IDS]               values, read from the module
  stored [IDS]            values as last known, without asking the module
  set ID=VALUE...         set values and send them to the bus; VALUE is JSON: true, 21.5,
                          {"control":true,"value":false}, "text"
  put ID=VALUE...         set values without sending them
  raw ID=HEX...           set raw values and send them
  read IDS                send read requests; the responses come as broadcasts
  progmode [on|off]       programming mode
  items                   server items
  watch                   print values as they change, until interrupted
  version, reset          version of the server; reload the datapoints
  call METHOD [JSON]      any request of the JSON API
IDS are numbers, ranges (1-10), names or *, separated by spaces or commas.
`

var printJSON bool

func main() {
	socket := flag.String("s", envOr("BAOSCTL_SOCKET", "/tmp/knxgw.json"), "unix socket of knxgw -o json (env BAOSCTL_SOCKET)")
	flag.BoolVar(&printJSON, "json", false, "print the payloads as JSON")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: baosctl [options] [COMMAND [ARGS...]]\n\n%s\nOptions:\n", help)
		flag.PrintDefaults()
	}
	flag.Parse()

	c, err := sdk.Dial(*socket)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer c.Close()

	if flag.NArg() > 0 {
		if err := run(c, flag.Args()); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}

		return
	}

	interactive(c)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return def
}

func interactive(c *sdk.Client) {
	var done atomic.Bool

	go func() {
		for m := range c.Broadcasts() {
			printBroadcast(m)
		}

		if !done.Load() {
			fmt.Println("The connection to knxgw closed")
			os.Exit(1)
		}
	}()
	defer done.Store(true)

	info, _ := os.Stdin.Stat()
	terminal := info != nil && info.Mode()&os.ModeCharDevice != 0
	prompt := func() {
		if terminal {
			fmt.Print("> ")
		}
	}

	prompt()

	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		if args := splitArgs(scanner.Text()); len(args) > 0 {
			if err := run(c, args); err != nil {
				fmt.Println(err)
			}
		}

		prompt()
	}
}

// splitArgs splits a line at spaces, except inside quotes or braces, so that JSON values stay
// whole: set 9={"day": 1, "hour": 12}.
func splitArgs(line string) []string {
	var (
		args  []string
		cur   strings.Builder
		depth int
		quote bool
	)

	for _, r := range line {
		switch {
		case r == '"':
			quote = !quote
		case !quote && (r == '{' || r == '['):
			depth++
		case !quote && (r == '}' || r == ']'):
			depth--
		case !quote && depth == 0 && (r == ' ' || r == '\t'):
			if cur.Len() > 0 {
				args = append(args, cur.String())
				cur.Reset()
			}

			continue
		}

		cur.WriteRune(r)
	}

	if cur.Len() > 0 {
		args = append(args, cur.String())
	}

	return args
}

func run(c *sdk.Client, args []string) error {
	cmd, args := args[0], args[1:]

	switch cmd {
	case "description", "descr", "d":
		return call(c, "get description", ids(args), printDescriptions)

	case "get", "g":
		return call(c, "get value", ids(args), printValues)

	case "stored":
		return call(c, "get stored", ids(args), printValues)

	case "set", "put", "raw":
		values, err := assignments(args, cmd == "raw")
		if err != nil {
			return err
		}

		method := "set value"
		if cmd == "put" {
			method = "put value"
		}

		return call(c, method, values, printValues)

	case "read":
		if len(args) == 0 {
			return fmt.Errorf("read which datapoints?")
		}

		return call(c, "read value", ids(args), func(json.RawMessage) error { return nil })

	case "progmode":
		if len(args) == 0 {
			return call(c, "get programming mode", nil, printPlain)
		}

		on := args[0] == "on" || args[0] == "1" || args[0] == "true"

		return call(c, "set programming mode", on, printPlain)

	case "items":
		return call(c, "get server items", nil, printItems)

	case "version":
		return call(c, "version", nil, printPlain)

	case "reset":
		return call(c, "reset", nil, printPlain)

	case "watch":
		for m := range c.Broadcasts() {
			printBroadcast(m)
		}

		return fmt.Errorf("the connection to knxgw closed")

	case "call":
		if len(args) == 0 {
			return fmt.Errorf("call which method?")
		}

		var payload any
		if len(args) > 1 {
			if err := json.Unmarshal([]byte(strings.Join(args[1:], " ")), &payload); err != nil {
				return fmt.Errorf("payload: %v", err)
			}
		}

		return call(c, args[0], payload, func(p json.RawMessage) error {
			fmt.Println(string(p))
			return nil
		})

	case "help", "h", "?":
		fmt.Print(help)
		return nil
	}

	return fmt.Errorf("unknown command %q; try help", cmd)
}

func call(c *sdk.Client, method string, payload any, print func(json.RawMessage) error) error {
	res, err := c.Call(method, payload)
	if err != nil {
		return err
	}

	if printJSON {
		fmt.Println(string(res))
		return nil
	}

	return print(res)
}

// ids returns the payload of datapoint IDs: null for all, else an array of numbers and names.
func ids(args []string) any {
	var out []any

	for _, arg := range args {
		for _, s := range strings.Split(arg, ",") {
			if s == "" {
				continue
			}

			if s == "*" {
				return nil
			}

			if low, high, ok := strings.Cut(s, "-"); ok {
				a, err1 := strconv.Atoi(low)
				b, err2 := strconv.Atoi(high)

				if err1 == nil && err2 == nil {
					for id := a; id <= b; id++ {
						out = append(out, id)
					}

					continue
				}
			}

			if id, err := strconv.Atoi(s); err == nil {
				out = append(out, id)
			} else {
				out = append(out, s)
			}
		}
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

// assignments returns the payload of ID=VALUE arguments: VALUE is JSON, or text if it is not
// valid JSON; with raw, it is hex.
func assignments(args []string, raw bool) ([]map[string]any, error) {
	var out []map[string]any

	for _, arg := range args {
		key, text, ok := strings.Cut(arg, "=")
		if !ok {
			return nil, fmt.Errorf("%q: expected ID=VALUE", arg)
		}

		var id any = key
		if n, err := strconv.Atoi(key); err == nil {
			id = n
		}

		m := map[string]any{"id": id}

		if raw {
			data, err := hex.DecodeString(strings.ReplaceAll(text, " ", ""))
			if err != nil {
				return nil, fmt.Errorf("%q: %v", arg, err)
			}

			m["raw"] = data // base64 in JSON
		} else {
			var value any
			if json.Unmarshal([]byte(text), &value) != nil {
				value = text
			}

			m["value"] = value
		}

		out = append(out, m)
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("set what? ID=VALUE")
	}

	return out, nil
}

func printPlain(p json.RawMessage) error {
	fmt.Println(string(p))
	return nil
}

func printDescriptions(p json.RawMessage) error {
	var list []sdk.Description
	if err := json.Unmarshal(p, &list); err != nil {
		return err
	}

	for _, d := range list {
		flags := []byte("CRWTUI")
		for i, on := range []bool{d.Communication, d.Read, d.Write, d.Transmit, d.Update, d.ReadOnInit} {
			if !on {
				flags[i] = '-'
			}
		}

		fmt.Printf("%5d  %-7s %3d bits  %s  %-6s  %s\n", d.ID, d.Type, d.Size, flags, d.Priority, d.Name)
	}

	fmt.Printf("%d datapoints\n", len(list))

	return nil
}

type value struct {
	ID      uint16          `json:"id"`
	Value   json.RawMessage `json:"value"`
	Raw     []byte          `json:"raw"`
	Valid   *bool           `json:"valid"`
	Success *bool           `json:"success"`
}

func (v value) String() string {
	s := fmt.Sprintf("%5d  % x", v.ID, v.Raw)
	if len(v.Value) > 0 {
		s = fmt.Sprintf("%5d  %s  (% x)", v.ID, v.Value, v.Raw)
	}

	if v.Valid != nil && !*v.Valid {
		s += "  not from the bus"
	}

	return s
}

func printValues(p json.RawMessage) error {
	var list []value
	if err := json.Unmarshal(p, &list); err != nil {
		return err
	}

	for _, v := range list {
		fmt.Println(v)
	}

	return nil
}

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
		fmt.Printf("%5d  %-20s % x\n", item.ID, itemNames[item.ID], item.Raw)
	}

	return nil
}

func printBroadcast(m sdk.Message) {
	if printJSON {
		fmt.Printf("%s %s\n", m.Method, m.Payload)
		return
	}

	switch m.Method {
	case "datapoint value":
		var list []value
		json.Unmarshal(m.Payload, &list)

		for _, v := range list {
			fmt.Println("value", v)
		}

	case "server item":
		var list []sdk.Item
		json.Unmarshal(m.Payload, &list)

		for _, item := range list {
			fmt.Printf("item %5d  %-20s % x\n", item.ID, itemNames[item.ID], item.Raw)
		}

	default:
		fmt.Printf("%s %s\n", m.Method, m.Payload)
	}
}
