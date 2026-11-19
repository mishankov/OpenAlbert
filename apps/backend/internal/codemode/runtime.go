// Package codemode executes model-written JavaScript and returns authorized tool
// requests to the caller. The VM's only application import queues requests. It
// does not execute tools or receive a Go conversation object.
package codemode

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

const (
	MaxRequests        = 64
	MaxArgumentsBytes  = 100_000
	MaxOutputBytes     = 32 << 10
	MaxStateBytes      = 16 << 10
	MaxCodeBytes       = 32 << 10
	maxInputBytes      = 512 << 10
	maxHistoryBytes    = 4 << 20
	wasmMemoryPages    = 512 // 32 MiB
	executionTimeLimit = 2 * time.Second
)

//go:embed guest/quickjs.wasm
var wasm []byte

//go:embed guest/bootstrap.js
var bootstrap []byte

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type Request struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type Receipt struct {
	Request Request
	Result  json.RawMessage
	Failed  bool
}

type Input struct {
	Code      string
	Tools     []Tool
	History   [][]Receipt
	State     map[string]json.RawMessage
	StartedAt time.Time
}

type Result struct {
	Pending []Request
	Output  json.RawMessage
	State   map[string]json.RawMessage
	Error   string
}

var loadEngine = sync.OnceValues(func() (engine, error) {
	ctx := context.Background()
	runtime := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().
		WithCloseOnContextDone(true).
		WithMemoryLimitPages(wasmMemoryPages))
	compiled, err := runtime.CompileModule(ctx, wasm)
	if err == nil {
		err = validateImports(compiled)
	}
	if err == nil {
		_, err = wasi_snapshot_preview1.Instantiate(ctx, runtime)
	}
	if err == nil {
		_, err = runtime.NewHostModuleBuilder("openalbert").NewFunctionBuilder().
			WithFunc(enqueueTool).Export("enqueue_tool").Instantiate(ctx)
	}
	if err != nil {
		_ = runtime.Close(ctx)
		return engine{}, err
	}
	return engine{runtime: runtime, compiled: compiled}, nil
})

type engine struct {
	runtime  wazero.Runtime
	compiled wazero.CompiledModule
}

func validateImports(compiled wazero.CompiledModule) error {
	// The QuickJS engine uses WASI libc for its fixed clock and standard
	// streams. Reject any added imports before linking the host functions.
	if len(compiled.ImportedMemories()) != 0 {
		return errors.New("Codemode guest must provide its own memory")
	}
	for _, definition := range compiled.ImportedFunctions() {
		module, name, _ := definition.Import()
		switch {
		case module == "openalbert" && name == "enqueue_tool":
			continue
		case module == wasi_snapshot_preview1.ModuleName:
			switch name {
			case "clock_time_get", "fd_close", "fd_fdstat_get", "fd_seek", "fd_write":
				continue
			}
		}
		return fmt.Errorf("unexpected Codemode guest import %s.%s", module, name)
	}
	return nil
}

type requestQueueKey struct{}

type requestQueue struct {
	allowed  map[string]bool
	requests []Request
	err      string
}

func enqueueTool(ctx context.Context, module api.Module, namePointer, nameSize, argsPointer, argsSize uint32) int32 {
	queue := ctx.Value(requestQueueKey{}).(*requestQueue)
	switch {
	case len(queue.requests) >= MaxRequests:
		queue.err = "Codemode tool call limit exceeded"
	case nameSize > 128 || argsSize > MaxArgumentsBytes:
		queue.err = "Codemode tool name or arguments exceed the size limit"
	default:
		name, nameOK := module.Memory().Read(namePointer, nameSize)
		args, argsOK := module.Memory().Read(argsPointer, argsSize)
		if !nameOK || !argsOK || !queue.allowed[string(name)] {
			queue.err = "Codemode tool unavailable"
		} else if clean := bytes.TrimSpace(args); len(clean) == 0 || clean[0] != '{' || !json.Valid(clean) {
			queue.err = "Codemode tool arguments must be a JSON object"
		} else {
			id := int32(len(queue.requests))
			queue.requests = append(queue.requests, Request{
				Name:      string(name),
				Arguments: append(json.RawMessage(nil), args...),
			})
			return id
		}
	}
	return -1
}

type vm struct {
	ctx    context.Context
	module api.Module
}

func (v vm) call(name string, args ...uint64) (uint64, error) {
	fn := v.module.ExportedFunction(name)
	res, err := fn.Call(v.ctx, args...)
	if err != nil {
		return 0, err
	}
	if len(res) == 0 {
		return 0, nil
	}
	return res[0], nil
}

func (v vm) send(function string, data []byte, args ...uint64) error {
	// QuickJS's evaluator also expects the end of the source buffer to be NUL.
	ptr, err := v.call("allocate", uint64(len(data)+1))
	if err != nil {
		return err
	}
	if ptr == 0 {
		return errors.New("Codemode guest buffer allocation failed")
	}
	defer func() { _, _ = v.call("release", ptr) }()
	if !v.module.Memory().Write(uint32(ptr), data) ||
		!v.module.Memory().WriteByte(uint32(ptr)+uint32(len(data)), 0) {
		return errors.New("Codemode guest buffer is out of bounds")
	}
	code, err := v.call(function, append(args, ptr, uint64(len(data)))...)
	if err != nil {
		return err
	}
	if int32(code) != 0 {
		detail, _ := v.result()
		if len(detail) > 1024 {
			detail = detail[:1024]
		}
		if len(detail) == 0 {
			detail = []byte("guest execution failed")
		}
		return fmt.Errorf("Codemode: %s", detail)
	}
	return nil
}

func (v vm) result() ([]byte, error) {
	ptr, err := v.call("result_pointer")
	if err != nil {
		return nil, err
	}
	size, err := v.call("result_length")
	if err != nil {
		return nil, err
	}
	if size == 0 {
		return nil, errors.New("Codemode result is empty or exceeds the size limit")
	}
	value, ok := v.module.Memory().Read(uint32(ptr), uint32(size))
	if !ok {
		return nil, errors.New("Codemode result is out of bounds")
	}
	return value, nil
}

type snapshot struct {
	Status string                     `json:"status"`
	Output json.RawMessage            `json:"output"`
	State  map[string]json.RawMessage `json:"state"`
	Error  string                     `json:"error"`
}

const (
	startOperation = iota
	settleOperation
	snapshotOperation
)

type settlement struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Failed bool            `json:"failed"`
}

// Run replays the script up to the next unresolved batch, or returns the final
// output and staged session state. History records the exact sequence of tool
// results delivered to the script. Each call uses a fresh guest instance.
func Run(ctx context.Context, in Input) (Result, error) {
	settings, err := encodeInput(in)
	if err != nil {
		return Result{Error: err.Error()}, nil
	}
	engine, err := loadEngine()
	if err != nil {
		return Result{}, fmt.Errorf("initialize Codemode WebAssembly engine: %w", err)
	}
	vmContext, cancel := context.WithTimeout(ctx, executionTimeLimit)
	defer cancel()
	queue := &requestQueue{allowed: make(map[string]bool, len(in.Tools))}
	for _, tool := range in.Tools {
		queue.allowed[tool.Name] = true
	}
	vmContext = context.WithValue(vmContext, requestQueueKey{}, queue)
	stamp := in.StartedAt
	if stamp.IsZero() {
		stamp = time.Unix(0, 0)
	}
	moduleConfig := wazero.NewModuleConfig().WithName("").WithStartFunctions("_initialize").
		WithWalltime(func() (int64, int32) { return stamp.Unix(), int32(stamp.Nanosecond()) }, 1_000_000).
		WithRandSource(rand.New(rand.NewSource(stamp.UnixNano())))
	module, err := engine.runtime.InstantiateModule(vmContext, engine.compiled, moduleConfig)
	if err != nil {
		return Result{}, fmt.Errorf("instantiate Codemode WebAssembly module: %w", err)
	}
	defer func() { _ = module.Close(context.Background()) }()
	result, runErr := run(vm{ctx: vmContext, module: module}, queue, settings, in.History)
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}
	if vmContext.Err() != nil {
		return Result{Error: "Codemode script execution deadline exceeded"}, nil
	}
	if runErr != nil {
		return Result{Error: runErr.Error()}, nil
	}
	return result, nil
}

func encodeInput(in Input) ([]byte, error) {
	if len(in.Code) > MaxCodeBytes {
		return nil, errors.New("Codemode script exceeds the size limit")
	}
	if len(in.History) > MaxRequests {
		return nil, errors.New("Codemode history exceeds the call limit")
	}
	calls := 0
	size := 0
	for _, batch := range in.History {
		calls += len(batch)
		if len(batch) == 0 || calls > MaxRequests {
			return nil, errors.New("invalid Codemode history batch")
		}
		for _, receipt := range batch {
			size += len(receipt.Request.Name) + len(receipt.Request.Arguments) + len(receipt.Result)
		}
	}
	if size > maxHistoryBytes {
		return nil, errors.New("Codemode history exceeds the size limit")
	}
	state, err := json.Marshal(in.State)
	if err != nil || len(state) > MaxStateBytes {
		return nil, errors.New("invalid or oversized Codemode session state")
	}
	catalog := in.Tools
	if catalog == nil {
		catalog = []Tool{}
	}
	settings, err := json.Marshal(struct {
		Code  string          `json:"code"`
		Tools []Tool          `json:"tools"`
		State json.RawMessage `json:"state"`
	}{in.Code, catalog, state})
	if err != nil || len(settings) > maxInputBytes {
		return nil, errors.New("invalid or oversized Codemode input")
	}
	return settings, nil
}

func run(v vm, queue *requestQueue, settings []byte, history [][]Receipt) (Result, error) {
	if err := v.send("initialize", bootstrap); err != nil {
		return Result{}, err
	}
	if err := v.send("invoke", settings, startOperation); err != nil {
		return Result{}, err
	}
	seen := 0
	for _, batch := range history {
		if queue.err != "" || len(queue.requests)-seen != len(batch) {
			return Result{}, errors.New("Codemode replay produced a different tool batch")
		}
		records := make([]settlement, 0, len(batch))
		for _, receipt := range batch {
			request := queue.requests[seen]
			if request.Name != receipt.Request.Name || !equalJSON(request.Arguments, receipt.Request.Arguments) {
				return Result{}, errors.New("Codemode replay produced a different tool request")
			}
			records = append(records, settlement{ID: seen, Result: receipt.Result, Failed: receipt.Failed})
			seen++
		}
		data, err := json.Marshal(records)
		if err != nil {
			return Result{}, fmt.Errorf("encode Codemode tool receipts: %w", err)
		}
		if err := v.send("invoke", data, settleOperation); err != nil {
			return Result{}, err
		}
	}
	if err := v.send("invoke", nil, snapshotOperation); err != nil {
		return Result{}, err
	}
	raw, err := v.result()
	if err != nil {
		return Result{}, err
	}
	var state snapshot
	if err := json.Unmarshal(raw, &state); err != nil {
		return Result{}, fmt.Errorf("decode Codemode result: %w", err)
	}
	if len(state.Output) > MaxOutputBytes {
		return Result{}, errors.New("Codemode output limit exceeded")
	}
	staged, err := json.Marshal(state.State)
	if err != nil || len(staged) > MaxStateBytes {
		return Result{}, errors.New("Codemode store limit exceeded")
	}
	if queue.err != "" {
		return Result{Output: state.Output, Error: queue.err}, nil
	}
	switch state.Status {
	case "running":
		if seen == len(queue.requests) {
			return Result{Output: state.Output, Error: "Codemode blocked on a promise without a pending tool call"}, nil
		}
		return Result{Pending: queue.requests[seen:]}, nil
	case "failed":
		return Result{Output: state.Output, Error: state.Error}, nil
	case "completed":
		return Result{Output: state.Output, State: state.State}, nil
	default:
		return Result{}, errors.New("invalid Codemode guest status")
	}
}

func equalJSON(a, b json.RawMessage) bool {
	if bytes.Equal(a, b) {
		return true
	}
	decode := func(raw json.RawMessage) (any, error) {
		if !json.Valid(raw) {
			return nil, errors.New("invalid JSON")
		}
		var value any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		err := decoder.Decode(&value)
		return value, err
	}
	left, leftErr := decode(a)
	right, rightErr := decode(b)
	return leftErr == nil && rightErr == nil && reflect.DeepEqual(left, right)
}
