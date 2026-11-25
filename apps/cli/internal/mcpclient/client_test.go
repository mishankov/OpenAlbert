package mcpclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConnectHTTPMCPServerAndCallTool(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "sess-1")
		switch req.Method {
		case "initialize":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2024-11-05"}})
		case "notifications/initialized":
			w.WriteHeader(202)
		case "tools/list":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{
					"tools": []map[string]any{{
						"name":        "ping",
						"description": "Echo a message",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"msg": map[string]string{"type": "string"}}},
					}},
				},
			})
		case "tools/call":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{
					"content": []map[string]string{{"type": "text", "text": `{"pong":"ok"}`}},
				},
			})
		}
	}))
	defer srv.Close()

	mgr, warnings := Connect(context.Background(), []ServerConfig{{Name: "t3-code", Type: "http", URL: srv.URL}}, nil)
	defer mgr.Close()
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	defs := mgr.Definitions()
	if len(defs) != 1 || defs[0].Name != "mcp_t3_code_ping" {
		t.Fatalf("unexpected MCP definitions: %+v", defs)
	}
	out, failed, _ := mgr.Call(context.Background(), "mcp_t3_code_ping", json.RawMessage(`{"msg":"hi"}`))
	if failed || !strings.Contains(string(out), `"pong":"ok"`) {
		t.Fatalf("MCP tool call failed: %s failed=%v", out, failed)
	}
}

func TestHTTPMCPSSELargePayloadAndMissingResponseErrors(t *testing.T) {
	largeText := strings.Repeat("M", 75*1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "text/event-stream")
		switch req.Method {
		case "initialize":
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2024-11-05"}})
			_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
		case "notifications/initialized":
			w.WriteHeader(202)
		case "tools/list":
			b, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"tools": []map[string]any{
					{"name": "big", "description": "Big SSE reply", "inputSchema": map[string]any{"type": "object"}},
					{"name": "multiline", "description": "Multi-line SSE reply", "inputSchema": map[string]any{"type": "object"}},
					{"name": "ping_collision", "description": "Server ping with same id before result", "inputSchema": map[string]any{"type": "object"}},
					{"name": "missing", "description": "Missing reply", "inputSchema": map[string]any{"type": "object"}},
					{"name": "noid", "description": "JSON missing id", "inputSchema": map[string]any{"type": "object"}},
				}},
			})
			_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
		case "tools/call":
			var p struct {
				Name string `json:"name"`
			}
			raw, _ := json.Marshal(req.Params)
			_ = json.Unmarshal(raw, &p)
			if p.Name == "missing" {
				// Emit an unrelated notification then close without a matching response ID.
				_, _ = w.Write([]byte("data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n"))
				return
			}
			if p.Name == "ping_collision" {
				// Emit a server-initiated ping request with the SAME numeric id before the real response.
				pingB, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "method": "ping"})
				respB, _ := json.Marshal(map[string]any{
					"jsonrpc": "2.0", "id": req.ID,
					"result": map[string]any{"content": []map[string]string{{"type": "text", "text": `{"real":"after_ping"}`}}},
				})
				_, _ = w.Write([]byte("data: " + string(pingB) + "\n\ndata: " + string(respB) + "\n\n"))
				return
			}
			if p.Name == "multiline" {
				// Split one valid JSON-RPC SSE event across two data: lines.
				b, _ := json.Marshal(map[string]any{
					"jsonrpc": "2.0", "id": req.ID,
					"result": map[string]any{"content": []map[string]string{{"type": "text", "text": `{"split":"ok"}`}}},
				})
				mid := len(b) / 2
				_, _ = w.Write([]byte("data: " + string(b[:mid]) + "\ndata: " + string(b[mid:]) + "\n\n"))
				return
			}
			if p.Name == "noid" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":{"content":[{"type":"text","text":"oops"}]}}`))
				return
			}
			b, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"content": []map[string]string{{"type": "text", "text": largeText}}},
			})
			_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
		}
	}))
	defer srv.Close()

	mgr, warnings := Connect(context.Background(), []ServerConfig{{Name: "sse", Type: "http", URL: srv.URL}}, nil)
	defer mgr.Close()
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	out, failed, _ := mgr.Call(context.Background(), "mcp_sse_big", json.RawMessage(`{}`))
	if failed || !strings.Contains(string(out), largeText[:1024]) {
		t.Fatalf("expected >70KB SSE response to succeed, failed=%v len=%d", failed, len(out))
	}
	out, failed, _ = mgr.Call(context.Background(), "mcp_sse_multiline", json.RawMessage(`{}`))
	if failed || !strings.Contains(string(out), `"split":"ok"`) {
		t.Fatalf("expected multi-line SSE event to assemble and succeed, failed=%v out=%s", failed, out)
	}
	out, failed, _ = mgr.Call(context.Background(), "mcp_sse_ping_collision", json.RawMessage(`{}`))
	if failed || !strings.Contains(string(out), `"real":"after_ping"`) {
		t.Fatalf("expected server ping with colliding id to be ignored in favor of real response, failed=%v out=%s", failed, out)
	}
	out, failed, _ = mgr.Call(context.Background(), "mcp_sse_missing", json.RawMessage(`{}`))
	if !failed {
		t.Fatalf("expected SSE stream without matching response ID to fail, got %s", out)
	}
	out, failed, _ = mgr.Call(context.Background(), "mcp_sse_noid", json.RawMessage(`{}`))
	if !failed {
		t.Fatalf("expected JSON response with missing id to fail, got %s", out)
	}
}

func TestStdioMCPIgnoresServerPingWithCollidingID(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	defer inW.Close()
	defer outR.Close()

	sc := &stdioConn{
		stdin:   outW,
		pending: map[int64]chan rpcResponse{},
		closed:  make(chan struct{}),
	}
	go sc.readLoop(inR)

	// Drain outbound request, send a server-initiated ping with colliding id=1,
	// WAIT for the client's ping reply ({"jsonrpc":"2.0","id":1,"result":{}}),
	// and only then send the real tool call response.
	pingReplied := make(chan bool, 1)
	go func() {
		dec := json.NewDecoder(outR)
		var first map[string]any
		_ = dec.Decode(&first)
		_, _ = inW.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n"))
		var pingResp struct {
			ID     int64          `json:"id"`
			Result map[string]any `json:"result"`
		}
		if err := dec.Decode(&pingResp); err == nil && pingResp.ID == 1 && pingResp.Result != nil {
			pingReplied <- true
		} else {
			pingReplied <- false
		}
		_, _ = inW.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}` + "\n"))
	}()

	var res struct {
		OK bool `json:"ok"`
	}
	if err := sc.Call(context.Background(), "tools/call", map[string]any{}, &res); err != nil || !res.OK {
		t.Fatalf("stdio Call failed after server ping: res=%+v err=%v", res, err)
	}
	if ok := <-pingReplied; !ok {
		t.Fatal("stdio MCP client did not reply to server-initiated ping with original ID and empty result")
	}
}

func TestConnectHTTPMCPFollowsNextCursorPagination(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2024-11-05"}})
		case "notifications/initialized":
			w.WriteHeader(202)
		case "tools/list":
			var p struct {
				Cursor string `json:"cursor"`
			}
			raw, _ := json.Marshal(req.Params)
			_ = json.Unmarshal(raw, &p)
			if p.Cursor == "" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0", "id": req.ID,
					"result": map[string]any{
						"tools": []map[string]any{{
							"name":        "page_one",
							"description": "Tool from page 1",
							"inputSchema": map[string]any{"type": "object"},
						}},
						"nextCursor": "cursor-2",
					},
				})
				return
			}
			if p.Cursor == "cursor-2" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0", "id": req.ID,
					"result": map[string]any{
						"tools": []map[string]any{{
							"name":        "page_two",
							"description": "Tool from page 2",
							"inputSchema": map[string]any{"type": "object"},
						}},
					},
				})
				return
			}
			http.Error(w, "unexpected cursor", http.StatusBadRequest)
		case "tools/call":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{
					"content": []map[string]string{{"type": "text", "text": `{"page":2}`}},
				},
			})
		}
	}))
	defer srv.Close()

	mgr, warnings := Connect(context.Background(), []ServerConfig{{Name: "paged", Type: "http", URL: srv.URL}}, nil)
	defer mgr.Close()
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	defs := mgr.Definitions()
	if len(defs) != 2 || defs[0].Name != "mcp_paged_page_one" || defs[1].Name != "mcp_paged_page_two" {
		t.Fatalf("expected tools from both pages, got: %+v", defs)
	}
	out, failed, _ := mgr.Call(context.Background(), "mcp_paged_page_two", json.RawMessage(`{}`))
	if failed || !strings.Contains(string(out), `"page":2`) {
		t.Fatalf("expected page 2 tool to be callable, failed=%v out=%s", failed, out)
	}
}

func TestConnectHTTPMCPStrictHandshakeAcceptHeaderAndNotifyError(t *testing.T) {
	// Strict server requires Accept with both application/json and text/event-stream on every POST,
	// including notifications/initialized, and only allows tools/list after notifications/initialized succeeds.
	initialized := false
	strictSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept := r.Header.Get("Accept")
		if !strings.Contains(accept, "application/json") || !strings.Contains(accept, "text/event-stream") {
			http.Error(w, "missing required Accept media types", http.StatusNotAcceptable)
			return
		}
		var req rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2024-11-05"}})
		case "notifications/initialized":
			initialized = true
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			if !initialized {
				http.Error(w, "not initialized", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{
					"tools": []map[string]any{{"name": "strict_ok", "inputSchema": map[string]any{"type": "object"}}},
				},
			})
		}
	}))
	defer strictSrv.Close()

	mgr, warnings := Connect(context.Background(), []ServerConfig{{Name: "strict", Type: "http", URL: strictSrv.URL}}, nil)
	defer mgr.Close()
	if len(warnings) != 0 {
		t.Fatalf("strict handshake failed with warnings: %v", warnings)
	}
	if !mgr.Has("mcp_strict_strict_ok") {
		t.Fatalf("expected tool from strict MCP server, got %+v", mgr.Definitions())
	}

	// Server that rejects notifications/initialized with 406 must cause Connect to abort before tools/list.
	listedAfterNotifyFail := false
	rejectSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2024-11-05"}})
		case "notifications/initialized":
			http.Error(w, "not acceptable", http.StatusNotAcceptable)
		case "tools/list":
			listedAfterNotifyFail = true
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"tools": []any{}}})
		}
	}))
	defer rejectSrv.Close()

	mgr2, warnings2 := Connect(context.Background(), []ServerConfig{{Name: "reject", Type: "http", URL: rejectSrv.URL}}, nil)
	defer mgr2.Close()
	if len(warnings2) == 0 {
		t.Fatal("expected Connect to report warning when notifications/initialized returns HTTP 406")
	}
	if listedAfterNotifyFail {
		t.Fatal("Connect called tools/list despite notifications/initialized failing with HTTP 406")
	}
}

func TestConnectHTTPMCPProtocolVersionNegotiationAndImageResult(t *testing.T) {
	const wantVersion = "2025-06-18"
	const pngB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+ip1sAAAAASUVORK5CYII="
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		if req.Method != "initialize" && r.Header.Get("MCP-Protocol-Version") != wantVersion {
			http.Error(w, "missing or wrong MCP-Protocol-Version", http.StatusBadRequest)
			return
		}
		switch req.Method {
		case "initialize":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": wantVersion}})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{
					"tools": []map[string]any{{"name": "screenshot", "inputSchema": map[string]any{"type": "object"}}},
				},
			})
		case "tools/call":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{
					"content": []map[string]any{{
						"type":     "image",
						"mimeType": "image/png",
						"data":     pngB64,
					}},
				},
			})
		}
	}))
	defer srv.Close()

	mgr, warnings := Connect(context.Background(), []ServerConfig{{Name: "screen", Type: "http", URL: srv.URL}}, nil)
	defer mgr.Close()
	if len(warnings) != 0 {
		t.Fatalf("2025-06-18 handshake failed: %v", warnings)
	}
	out, failed, images := mgr.Call(context.Background(), "mcp_screen_screenshot", json.RawMessage(`{}`))
	if failed {
		t.Fatalf("MCP screenshot tool call failed: %s", out)
	}
	if len(images) != 1 || images[0] != "data:image/png;base64,"+pngB64 {
		t.Fatalf("expected MCP image attachment to be preserved, got images=%+v out=%s", images, out)
	}
	if !strings.Contains(string(out), `"image_attached":true`) {
		t.Fatalf("expected image_attached metadata in MCP output, got %s", out)
	}
}


func TestCallRemoteInvalidParamsReturnsToolErrorNotTransport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2024-11-05"}})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{
					"tools": []map[string]any{{"name": "t3_thread_launch", "inputSchema": map[string]any{"type": "object"}}},
				},
			})
		case "tools/call":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"error": map[string]any{"code": -32602, "message": "Invalid parameters"},
			})
		}
	}))
	defer srv.Close()

	mgr, warnings := Connect(context.Background(), []ServerConfig{{Name: "t3-code", Type: "http", URL: srv.URL}}, nil)
	defer mgr.Close()
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	_, err := mgr.CallRemote(context.Background(), "t3_thread_launch", map[string]any{})
	if err == nil {
		t.Fatal("expected error for JSON-RPC -32602")
	}
	if strings.Contains(err.Error(), ErrMCPTransport.Error()) {
		t.Fatalf("JSON-RPC -32602 must NOT wrap ErrMCPTransport, got: %v", err)
	}
	var te *ToolError
	if !strings.Contains(err.Error(), "-32602") || func() bool { _, ok := err.(*ToolError); te, _ = err.(*ToolError); return !ok }() || te == nil {
		t.Fatalf("expected *ToolError with -32602, got %T: %v", err, err)
	}
}
