package acp

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("RPC error %d: %s", e.Code, e.Message) }

type message struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *rpcError        `json:"error,omitempty"`
}

type callResult struct {
	payload json.RawMessage
	err     error
}

type responder func(output any, err *rpcError)
type handler func(method string, params json.RawMessage, respond responder)

type conn struct {
	writer io.Writer
	wg     sync.WaitGroup
	mu     sync.Mutex
	nextID int64
	waits  map[int64]chan callResult
	closed bool
	handle handler
}

func newConn(reader io.Reader, writer io.Writer, handle handler) *conn {
	connection := &conn{writer: writer, waits: make(map[int64]chan callResult), handle: handle}
	connection.wg.Add(1)
	go connection.readLoop(reader)
	return connection
}

func (c *conn) call(method string, params any) (<-chan callResult, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errors.New("connection is closed")
	}
	c.nextID++
	id := c.nextID
	result := make(chan callResult, 1)
	c.waits[id] = result
	c.mu.Unlock()

	rawID := json.RawMessage(fmt.Sprintf("%d", id))
	if err := c.write(message{JSONRPC: "2.0", ID: &rawID, Method: method, Params: marshal(params)}); err != nil {
		c.mu.Lock()
		delete(c.waits, id)
		c.mu.Unlock()
		return nil, err
	}
	return result, nil
}

func (c *conn) notify(method string, params any) error {
	return c.write(message{JSONRPC: "2.0", Method: method, Params: marshal(params)})
}

func (c *conn) write(value message) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err = c.writer.Write(append(data, '\n'))
	return err
}

func (c *conn) readLoop(reader io.Reader) {
	defer c.wg.Done()
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64<<10), maxFrame)
	for scanner.Scan() {
		if len(scanner.Bytes()) == 0 {
			continue
		}
		var incoming message
		if json.Unmarshal(scanner.Bytes(), &incoming) == nil {
			c.route(incoming)
		}
	}
	readErr := scanner.Err()
	if readErr == nil {
		readErr = io.EOF
	}
	c.mu.Lock()
	c.closed = true
	for id, wait := range c.waits {
		wait <- callResult{err: fmt.Errorf("connection lost: %w", readErr)}
		delete(c.waits, id)
	}
	c.mu.Unlock()
}

func (c *conn) route(incoming message) {
	if incoming.Method == "" {
		c.routeResponse(incoming)
		return
	}
	if incoming.ID == nil {
		c.handle(incoming.Method, incoming.Params, nil)
		return
	}
	id := *incoming.ID
	go c.handle(incoming.Method, incoming.Params, func(output any, responseErr *rpcError) {
		response := message{JSONRPC: "2.0", ID: &id, Error: responseErr}
		if responseErr == nil {
			response.Result = marshal(output)
		}
		_ = c.write(response) //nolint:errcheck // The reader reports transport failure to pending calls.
	})
}

func (c *conn) routeResponse(incoming message) {
	if incoming.ID == nil {
		return
	}
	var id int64
	if json.Unmarshal(*incoming.ID, &id) != nil {
		return
	}
	c.mu.Lock()
	wait := c.waits[id]
	delete(c.waits, id)
	c.mu.Unlock()
	if wait == nil {
		return
	}
	if incoming.Error != nil {
		wait <- callResult{err: incoming.Error}
	} else {
		wait <- callResult{payload: incoming.Result}
	}
}

func marshal(value any) json.RawMessage {
	data, _ := json.Marshal(value) //nolint:errcheck // ACP protocol values contain JSON-safe fields.
	return data
}
