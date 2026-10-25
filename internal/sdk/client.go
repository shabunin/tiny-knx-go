package sdk

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"sync"
	"time"
)

// A Client talks to a Server.
type Client struct {
	conn       net.Conn
	broadcasts chan Message

	mu      sync.Mutex
	next    int
	pending map[string]chan Message
	err     error
}

// A Message is a response or a broadcast as a client gets it.
type Message struct {
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Payload json.RawMessage `json:"payload"`
}

// An Error is an error response.
type Error string

func (e Error) Error() string { return string(e) }

// Timeout is how long Call waits for a response.
const Timeout = 10 * time.Second

// Dial connects to the server on a unix socket.
func Dial(path string) (*Client, error) {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return nil, err
	}

	c := &Client{conn: conn, broadcasts: make(chan Message, 64), pending: map[string]chan Message{}}
	go c.read()

	return c, nil
}

// Broadcasts returns the broadcasts of the server. It closes when the connection closes. A client
// that does not read them misses them.
func (c *Client) Broadcasts() <-chan Message { return c.broadcasts }

// Close closes the connection.
func (c *Client) Close() error { return c.conn.Close() }

func (c *Client) read() {
	defer close(c.broadcasts)

	scanner := bufio.NewScanner(c.conn)
	scanner.Buffer(nil, 1<<20)

	for scanner.Scan() {
		var m Message
		if json.Unmarshal(scanner.Bytes(), &m) != nil {
			continue
		}

		if len(m.ID) == 0 {
			select {
			case c.broadcasts <- m:
			default:
			}

			continue
		}

		c.mu.Lock()
		ch := c.pending[string(m.ID)]
		delete(c.pending, string(m.ID))
		c.mu.Unlock()

		if ch != nil {
			ch <- m
		}
	}

	c.mu.Lock()
	c.err = errors.New("the connection to the server closed")
	for id, ch := range c.pending {
		delete(c.pending, id)
		close(ch)
	}
	c.mu.Unlock()
}

// Call sends a request and returns the payload of its response.
func (c *Client) Call(method string, payload any) (json.RawMessage, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return nil, c.err
	}

	c.next++
	id := strconv.Itoa(c.next)
	ch := make(chan Message, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	line, _ := json.Marshal(Request{ID: json.RawMessage(id), Method: method, Payload: data})
	if _, err := c.conn.Write(append(line, '\n')); err != nil {
		return nil, err
	}

	select {
	case m, ok := <-ch:
		if !ok {
			return nil, c.err
		}

		if m.Method == "error" {
			var code string
			json.Unmarshal(m.Payload, &code)

			return nil, Error(code)
		}

		return m.Payload, nil

	case <-time.After(Timeout):
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()

		return nil, Error("ERR_TIMEOUT")
	}
}
