package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

type rpcEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Transport handles bidirectional newline-delimited JSON-RPC 2.0 over stdio.
// Both client->agent requests and agent->client requests (session/request_permission)
// share the same reader/writer without deadlocking.
type Transport struct {
	in      io.Reader
	out     io.Writer
	writeMu sync.Mutex

	nextID    atomic.Int64
	pendingMu sync.Mutex
	pending   map[string]chan rpcEnvelope
}

func NewTransport(in io.Reader, out io.Writer) *Transport {
	return &Transport{
		in:      in,
		out:     out,
		pending: map[string]chan rpcEnvelope{},
	}
}

func (t *Transport) writeJSON(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	_, err = t.out.Write(append(raw, '\n'))
	return err
}

func (t *Transport) SendResult(id json.RawMessage, result any) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return t.writeJSON(rpcEnvelope{JSONRPC: "2.0", ID: id, Result: raw})
}

func (t *Transport) SendError(id json.RawMessage, code int, message string) error {
	return t.writeJSON(rpcEnvelope{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message}})
}

func (t *Transport) Notify(method string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return t.writeJSON(rpcEnvelope{JSONRPC: "2.0", Method: method, Params: raw})
}

// Call sends an agent->client JSON-RPC request (such as session/request_permission)
// and waits for the matching response.
func (t *Transport) Call(ctx context.Context, method string, params any, out any) error {
	idNum := t.nextID.Add(1)
	idKey := fmt.Sprintf("agent-%d", idNum)
	idRaw, _ := json.Marshal(idKey)
	paramsRaw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	ch := make(chan rpcEnvelope, 1)
	t.pendingMu.Lock()
	t.pending[idKey] = ch
	t.pendingMu.Unlock()

	if err := t.writeJSON(rpcEnvelope{JSONRPC: "2.0", ID: idRaw, Method: method, Params: paramsRaw}); err != nil {
		t.pendingMu.Lock()
		delete(t.pending, idKey)
		t.pendingMu.Unlock()
		return err
	}
	select {
	case resp, ok := <-ch:
		if !ok {
			return errors.New("ACP transport closed while awaiting client response")
		}
		if resp.Error != nil {
			return fmt.Errorf("client error %d: %s", resp.Error.Code, resp.Error.Message)
		}
		if out != nil && len(resp.Result) > 0 {
			return json.Unmarshal(resp.Result, out)
		}
		return nil
	case <-ctx.Done():
		t.pendingMu.Lock()
		delete(t.pending, idKey)
		t.pendingMu.Unlock()
		return ctx.Err()
	}
}

// Serve reads incoming JSON-RPC frames until EOF or ctx cancellation.
func (t *Transport) Serve(ctx context.Context, handle func(ctx context.Context, id json.RawMessage, method string, params json.RawMessage)) error {
	return t.ServeOrdered(ctx, nil, handle)
}

// ServeOrdered is like Serve, but invokes prepare synchronously in strict stdio
// receive order BEFORE dispatching each handler goroutine. Callers use prepare
// to register per-session operations (prompt, load, cancel) in arrival order so
// a subsequent session/cancel or session/load never races ahead of an earlier frame.
func (t *Transport) ServeOrdered(
	ctx context.Context,
	prepare func(ctx context.Context, method string, params json.RawMessage) context.Context,
	handle func(ctx context.Context, id json.RawMessage, method string, params json.RawMessage),
) error {
	serveCtx, cancelServe := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() {
		cancelServe()
		t.pendingMu.Lock()
		for k, ch := range t.pending {
			close(ch)
			delete(t.pending, k)
		}
		t.pendingMu.Unlock()
		wg.Wait()
	}()

	type readResult struct {
		line []byte
		err  error
		eof  bool
	}
	frames := make(chan readResult, 16)
	go func() {
		scanner := bufio.NewScanner(t.in)
		scanner.Buffer(make([]byte, 4096), 32<<20)
		for scanner.Scan() {
			line := append([]byte(nil), bytes.TrimSpace(scanner.Bytes())...)
			select {
			case frames <- readResult{line: line}:
			case <-serveCtx.Done():
				return
			}
		}
		if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
			select {
			case frames <- readResult{err: err}:
			case <-serveCtx.Done():
			}
			return
		}
		select {
		case frames <- readResult{eof: true}:
		case <-serveCtx.Done():
		}
	}()

	for {
		var res readResult
		select {
		case <-ctx.Done():
			return ctx.Err()
		case res = <-frames:
		}
		if res.err != nil {
			return res.err
		}
		if res.eof {
			return nil
		}
		if len(res.line) == 0 {
			continue
		}
		var env rpcEnvelope
		if err := json.Unmarshal(res.line, &env); err != nil {
			continue
		}
		// Incoming response to an outbound agent->client request.
		if env.Method == "" && len(env.ID) > 0 {
			var key string
			if json.Unmarshal(env.ID, &key) != nil {
				key = string(env.ID)
			}
			t.pendingMu.Lock()
			ch := t.pending[key]
			delete(t.pending, key)
			t.pendingMu.Unlock()
			if ch != nil {
				ch <- env
			}
			continue
		}
		if env.Method == "" {
			continue
		}
		idCopy := append(json.RawMessage(nil), env.ID...)
		paramsCopy := append(json.RawMessage(nil), env.Params...)
		method := env.Method
		callCtx := serveCtx
		if prepare != nil {
			callCtx = prepare(serveCtx, method, paramsCopy)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			handle(callCtx, idCopy, method, paramsCopy)
		}()
	}
}
