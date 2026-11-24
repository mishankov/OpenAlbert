package mcpclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"openalbert/apps/acp/internal/serverclient"
)

// ServerConfig matches the ACP session/new mcpServers entries (stdio or HTTP/SSE).
type ServerConfig struct {
	Type    string      `json:"type,omitempty"`
	Name    string      `json:"name"`
	Command string      `json:"command,omitempty"`
	Args    []string    `json:"args,omitempty"`
	Env     []EnvVar    `json:"env,omitempty"`
	URL     string      `json:"url,omitempty"`
	Headers []HTTPField `json:"headers,omitempty"`
}

type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type HTTPField struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

const latestMCPProtocolVersion = "2025-06-18"

func supportedMCPProtocolVersion(v string) (string, bool) {
	switch strings.TrimSpace(v) {
	case "2025-06-18", "2025-03-26", "2024-11-05":
		return strings.TrimSpace(v), true
	case "":
		return "2024-11-05", true
	default:
		return "", false
	}
}

type conn interface {
	Call(ctx context.Context, method string, params any, out any) error
	Notify(ctx context.Context, method string, params any) error
	SetProtocolVersion(version string)
	Close() error
}

type BoundTool struct {
	CodemodeName string
	ServerName   string
	RemoteName   string
	Schema       serverclient.Tool
	conn         conn
}

// Manager holds connected MCP servers for one ACP session and maps their tools
// into Codemode-safe tool names.
type Manager struct {
	mu    sync.Mutex
	conns []conn
	tools map[string]BoundTool
	order []string
}

var nonIdent = regexp.MustCompile(`[^a-zA-Z0-9_]+`)

func sanitizeIdent(s string) string {
	s = nonIdent.ReplaceAllString(strings.TrimSpace(s), "_")
	s = strings.Trim(s, "_")
	if s == "" {
		return "tool"
	}
	if s[0] >= '0' && s[0] <= '9' {
		s = "m_" + s
	}
	return s
}

// Connect connects to all configured MCP servers and discovers their tools.
// Individual server failures are returned in warnings so one bad server does
// not block the entire coding session.
func Connect(ctx context.Context, configs []ServerConfig, reservedNames map[string]bool) (*Manager, []string) {
	m := &Manager{tools: map[string]BoundTool{}}
	used := map[string]bool{}
	for k := range reservedNames {
		used[k] = true
	}
	var warnings []string
	for _, cfg := range configs {
		c, err := dialServer(ctx, cfg)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("mcp server %q failed to connect: %v", cfg.Name, err))
			continue
		}
		initCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		var initRes struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		err = c.Call(initCtx, "initialize", map[string]any{
			"protocolVersion": latestMCPProtocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]string{"name": "openalbert-acp", "version": "0.1.0"},
		}, &initRes)
		if err == nil {
			negotiated, ok := supportedMCPProtocolVersion(initRes.ProtocolVersion)
			if !ok {
				err = fmt.Errorf("unsupported MCP protocolVersion %q", initRes.ProtocolVersion)
			} else {
				c.SetProtocolVersion(negotiated)
			}
		}
		if err == nil {
			err = c.Notify(initCtx, "notifications/initialized", map[string]any{})
		}
		type listedTool struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		}
		var allTools []listedTool
		if err == nil {
			const maxToolListPages = 100
			seenCursors := map[string]bool{}
			var cursor string
			for page := 0; page < maxToolListPages; page++ {
				params := map[string]any{}
				if cursor != "" {
					params["cursor"] = cursor
				}
				var listRes struct {
					Tools      []listedTool `json:"tools"`
					NextCursor string       `json:"nextCursor"`
				}
				if err = c.Call(initCtx, "tools/list", params, &listRes); err != nil {
					break
				}
				allTools = append(allTools, listRes.Tools...)
				if listRes.NextCursor == "" {
					break
				}
				if seenCursors[listRes.NextCursor] {
					err = fmt.Errorf("tools/list pagination cursor loop (%q)", listRes.NextCursor)
					break
				}
				seenCursors[listRes.NextCursor] = true
				cursor = listRes.NextCursor
				if page == maxToolListPages-1 {
					err = fmt.Errorf("tools/list exceeded maximum %d pages", maxToolListPages)
				}
			}
		}
		cancel()
		if err != nil {
			_ = c.Close()
			warnings = append(warnings, fmt.Sprintf("mcp server %q initialization failed: %v", cfg.Name, err))
			continue
		}
		m.conns = append(m.conns, c)
		prefix := "mcp_" + sanitizeIdent(cfg.Name) + "_"
		for _, t := range allTools {
			params := t.InputSchema
			if len(params) == 0 || !json.Valid(params) {
				params = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			cmName := prefix + sanitizeIdent(t.Name)
			if used[cmName] {
				for i := 2; used[cmName]; i++ {
					cmName = fmt.Sprintf("%s%s_%d", prefix, sanitizeIdent(t.Name), i)
				}
			}
			used[cmName] = true
			desc := t.Description
			if desc == "" {
				desc = fmt.Sprintf("MCP tool %s from server %s", t.Name, cfg.Name)
			}
			m.tools[cmName] = BoundTool{
				CodemodeName: cmName,
				ServerName:   cfg.Name,
				RemoteName:   t.Name,
				Schema: serverclient.Tool{
					Name:        cmName,
					Description: fmt.Sprintf("[MCP %s] %s", cfg.Name, desc),
					Parameters:  params,
				},
				conn: c,
			}
			m.order = append(m.order, cmName)
		}
	}
	return m, warnings
}

func (m *Manager) Definitions() []serverclient.Tool {
	if m == nil {
		return nil
	}
	out := make([]serverclient.Tool, 0, len(m.order))
	for _, name := range m.order {
		out = append(out, m.tools[name].Schema)
	}
	return out
}

func (m *Manager) Has(name string) bool {
	if m == nil {
		return false
	}
	_, ok := m.tools[name]
	return ok
}

func (m *Manager) Call(ctx context.Context, codemodeName string, args json.RawMessage) (json.RawMessage, bool, []string) {
	bt, ok := m.tools[codemodeName]
	if !ok {
		raw, _ := json.Marshal(map[string]string{"error": "unknown MCP tool", "error_code": "unknown_tool"})
		return raw, true, nil
	}
	var parsed map[string]any
	if len(args) > 0 {
		_ = json.Unmarshal(args, &parsed)
	}
	if parsed == nil {
		parsed = map[string]any{}
	}
	var res struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text,omitempty"`
			Data     string `json:"data,omitempty"`
			MimeType string `json:"mimeType,omitempty"`
			Resource *struct {
				URI      string `json:"uri,omitempty"`
				Text     string `json:"text,omitempty"`
				Blob     string `json:"blob,omitempty"`
				MimeType string `json:"mimeType,omitempty"`
			} `json:"resource,omitempty"`
		} `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
		IsError           bool            `json:"isError,omitempty"`
	}
	err := bt.conn.Call(ctx, "tools/call", map[string]any{
		"name":      bt.RemoteName,
		"arguments": parsed,
	}, &res)
	if err != nil {
		raw, _ := json.Marshal(map[string]string{"error": err.Error(), "error_code": "mcp_error"})
		return raw, true, nil
	}
	var texts []string
	var images []string
	for _, c := range res.Content {
		switch c.Type {
		case "image":
			if data := strings.TrimSpace(c.Data); data != "" {
				mime := strings.TrimSpace(strings.Split(c.MimeType, ";")[0])
				if mime == "" {
					mime = "image/png"
				}
				images = append(images, fmt.Sprintf("data:%s;base64,%s", mime, data))
			}
		case "resource":
			if c.Resource != nil {
				if c.Resource.Text != "" {
					texts = append(texts, c.Resource.Text)
				} else if blob := strings.TrimSpace(c.Resource.Blob); blob != "" {
					mime := strings.ToLower(strings.TrimSpace(strings.Split(c.Resource.MimeType, ";")[0]))
					if strings.HasPrefix(mime, "image/") {
						images = append(images, fmt.Sprintf("data:%s;base64,%s", mime, blob))
					}
				}
			}
		default:
			if c.Text != "" {
				texts = append(texts, c.Text)
			}
		}
	}
	if len(res.StructuredContent) > 0 && json.Valid(res.StructuredContent) {
		return res.StructuredContent, res.IsError, images
	}
	joined := strings.Join(texts, "\n")
	if !res.IsError && json.Valid([]byte(joined)) && strings.HasPrefix(strings.TrimSpace(joined), "{") {
		return json.RawMessage(joined), false, images
	}
	if res.IsError {
		raw, _ := json.Marshal(map[string]string{"error": joined, "error_code": "mcp_tool_error"})
		return raw, true, nil
	}
	if len(images) > 0 {
		payload := map[string]any{"image_attached": true, "image_count": len(images)}
		if joined != "" {
			payload["text"] = joined
		}
		raw, _ := json.Marshal(payload)
		return raw, false, images
	}
	raw, _ := json.Marshal(map[string]string{"text": joined})
	return raw, false, nil
}

func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.conns {
		_ = c.Close()
	}
	m.conns = nil
}

func dialServer(ctx context.Context, cfg ServerConfig) (conn, error) {
	if cfg.URL != "" || cfg.Type == "http" || cfg.Type == "sse" {
		return &httpConn{
			url:     cfg.URL,
			headers: cfg.Headers,
			client:  &http.Client{Timeout: 60 * time.Second},
		}, nil
	}
	if cfg.Command == "" {
		return nil, errors.New("mcp server requires command or url")
	}
	return startStdio(ctx, cfg)
}

type stdioConn struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	writeMu sync.Mutex
	nextID  atomic.Int64

	pendingMu sync.Mutex
	pending   map[int64]chan rpcResponse
	closed    chan struct{}
}

func startStdio(_ context.Context, cfg ServerConfig) (*stdioConn, error) {
	cmd := exec.Command(cfg.Command, cfg.Args...)
	env := append([]string(nil), os.Environ()...)
	for _, e := range cfg.Env {
		env = append(env, e.Name+"="+e.Value)
	}
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	sc := &stdioConn{
		cmd:     cmd,
		stdin:   stdin,
		pending: map[int64]chan rpcResponse{},
		closed:  make(chan struct{}),
	}
	go sc.readLoop(stdout)
	return sc, nil
}

func (s *stdioConn) readLoop(r io.Reader) {
	defer close(s.closed)
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 16<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var resp rpcResponse
		if json.Unmarshal(line, &resp) != nil || len(resp.ID) == 0 {
			continue
		}
		if resp.Method == "ping" {
			// Respond to server-initiated ping per MCP 2024-11-05 utilities/ping
			// without disturbing any pending outbound client request.
			_ = s.writeJSON(map[string]any{
				"jsonrpc": "2.0",
				"id":      resp.ID,
				"result":  map[string]any{},
			})
			continue
		}
		if resp.Method != "" || (len(resp.Result) == 0 && resp.Error == nil) {
			continue
		}
		var id int64
		if json.Unmarshal(resp.ID, &id) != nil {
			continue
		}
		s.pendingMu.Lock()
		ch := s.pending[id]
		delete(s.pending, id)
		s.pendingMu.Unlock()
		if ch != nil {
			ch <- resp
		}
	}
}

func (s *stdioConn) writeJSON(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err = s.stdin.Write(append(raw, '\n'))
	return err
}

func (s *stdioConn) Call(ctx context.Context, method string, params any, out any) error {
	id := s.nextID.Add(1)
	ch := make(chan rpcResponse, 1)
	s.pendingMu.Lock()
	s.pending[id] = ch
	s.pendingMu.Unlock()
	if err := s.writeJSON(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		s.pendingMu.Lock()
		delete(s.pending, id)
		s.pendingMu.Unlock()
		return err
	}
	select {
	case resp := <-ch:
		if resp.Error != nil {
			return fmt.Errorf("mcp error %d: %s", resp.Error.Code, resp.Error.Message)
		}
		if out != nil && len(resp.Result) > 0 {
			return json.Unmarshal(resp.Result, out)
		}
		return nil
	case <-s.closed:
		return errors.New("mcp server exited")
	case <-ctx.Done():
		s.pendingMu.Lock()
		delete(s.pending, id)
		s.pendingMu.Unlock()
		return ctx.Err()
	}
}

func (s *stdioConn) Notify(_ context.Context, method string, params any) error {
	return s.writeJSON(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
}

func (s *stdioConn) SetProtocolVersion(string) {}

func (s *stdioConn) Close() error {
	_ = s.stdin.Close()
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	_ = s.cmd.Wait()
	return nil
}

type httpConn struct {
	url             string
	headers         []HTTPField
	client          *http.Client
	sessionID       string
	protocolVersion string
	nextID          atomic.Int64
	mu              sync.Mutex
}

func (h *httpConn) SetProtocolVersion(version string) {
	h.mu.Lock()
	h.protocolVersion = version
	h.mu.Unlock()
}

func (h *httpConn) Call(ctx context.Context, method string, params any, out any) error {
	id := h.nextID.Add(1)
	raw, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for _, f := range h.headers {
		req.Header.Set(f.Name, f.Value)
	}
	h.mu.Lock()
	if h.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", h.sessionID)
	}
	if h.protocolVersion != "" {
		req.Header.Set("MCP-Protocol-Version", h.protocolVersion)
	}
	h.mu.Unlock()
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		h.mu.Lock()
		h.sessionID = sid
		h.mu.Unlock()
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("mcp HTTP status %d", resp.StatusCode)
	}
	var rpcResp rpcResponse
	wantID := strconv.FormatInt(id, 10)
	matchesID := func(raw json.RawMessage) bool {
		trimmed := strings.TrimSpace(string(raw))
		if trimmed == wantID {
			return true
		}
		var asStr string
		return json.Unmarshal(raw, &asStr) == nil && asStr == wantID
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		scanner := bufio.NewScanner(io.LimitReader(resp.Body, 8<<20))
		scanner.Buffer(make([]byte, 64<<10), 8<<20)
		matched := false
		var dataLines []string
		dispatchEvent := func() bool {
			if len(dataLines) == 0 {
				return false
			}
			payload := strings.TrimSpace(strings.Join(dataLines, "\n"))
			dataLines = dataLines[:0]
			if payload == "" || payload == "[DONE]" {
				return false
			}
			var candidate rpcResponse
			if json.Unmarshal([]byte(payload), &candidate) == nil &&
				candidate.Method == "" &&
				(len(candidate.Result) > 0 || candidate.Error != nil) &&
				len(candidate.ID) > 0 && matchesID(candidate.ID) {
				rpcResp = candidate
				matched = true
				return true
			}
			return false
		}
		for scanner.Scan() {
			rawLine := strings.TrimRight(scanner.Text(), "\r")
			if rawLine == "" {
				if dispatchEvent() {
					break
				}
				continue
			}
			if strings.HasPrefix(rawLine, "data:") {
				val := strings.TrimPrefix(rawLine, "data:")
				if strings.HasPrefix(val, " ") {
					val = val[1:]
				}
				dataLines = append(dataLines, val)
			}
		}
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("mcp SSE read failed: %w", err)
		}
		if !matched {
			dispatchEvent()
		}
		if !matched {
			return errors.New("mcp SSE stream ended without a matching JSON-RPC response")
		}
	} else {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&rpcResp); err != nil {
			return err
		}
		if rpcResp.Method != "" || (len(rpcResp.Result) == 0 && rpcResp.Error == nil) || len(rpcResp.ID) == 0 || !matchesID(rpcResp.ID) {
			return fmt.Errorf("mcp response invalid or mismatched id: got %s, want %s", string(rpcResp.ID), wantID)
		}
	}
	if rpcResp.Error != nil {
		return fmt.Errorf("mcp error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	if out != nil && len(rpcResp.Result) > 0 {
		return json.Unmarshal(rpcResp.Result, out)
	}
	return nil
}

func (h *httpConn) Notify(ctx context.Context, method string, params any) error {
	raw, err := json.Marshal(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for _, f := range h.headers {
		req.Header.Set(f.Name, f.Value)
	}
	h.mu.Lock()
	if h.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", h.sessionID)
	}
	if h.protocolVersion != "" {
		req.Header.Set("MCP-Protocol-Version", h.protocolVersion)
	}
	h.mu.Unlock()
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		h.mu.Lock()
		h.sessionID = sid
		h.mu.Unlock()
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("mcp notify %s HTTP status %d: %s", method, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return nil
}

func (h *httpConn) Close() error { return nil }
