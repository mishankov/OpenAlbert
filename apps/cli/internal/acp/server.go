package acp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/mishankov/OpenAlbert/apps/cli/internal/agent"
	"github.com/mishankov/OpenAlbert/apps/cli/internal/bridge"
	"github.com/mishankov/OpenAlbert/apps/cli/internal/config"
	"github.com/mishankov/OpenAlbert/apps/cli/internal/mcpclient"
	"github.com/mishankov/OpenAlbert/apps/cli/internal/serverclient"
	"github.com/mishankov/OpenAlbert/apps/cli/internal/session"
	"github.com/mishankov/OpenAlbert/apps/cli/internal/skills"
	"github.com/mishankov/OpenAlbert/apps/cli/internal/tools"
)

const (
	Version                  = "0.1.0"
	SupportedProtocolVersion = 1
)

type sessionOpKind int

const (
	opPrompt sessionOpKind = iota
	opLoad
	opConfig
)

type promptHandle struct {
	sessionID  string
	kind       sessionOpKind
	ctx        context.Context
	cancel     context.CancelFunc
	prev       *promptHandle
	waitReload *promptHandle
	done       chan struct{}
	finishOnce sync.Once
}

type sessionOpKey struct{}

// Server implements the Agent Client Protocol (ACP) over stdio.
type Server struct {
	cfg       config.Config
	client    *serverclient.Client
	store     *session.Store
	transport *Transport
	logger    *slog.Logger

	mu            sync.Mutex
	bridge        *bridge.Bridge
	sessions      map[string]*agent.Session
	sessionOps    map[string]*promptHandle // Tail of serialized per-session ops (prompts, reloads, and post-reload config).
	activePrompts map[string]*promptHandle // Latest prompt handle per session for session/cancel.
	activeReloads map[string]*promptHandle // Latest in-flight reload handle per session for config setters.
	configOps     map[string]*promptHandle // Latest config op per session so reload waits for prior setters.
	models        []serverclient.CatalogModel
	defModel      string
}

func NewServer(cfg config.Config, in io.Reader, out io.Writer, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	return &Server{
		cfg:           cfg,
		client:        serverclient.New(cfg.ServerURL, cfg.Token, cfg.RequestTimeout),
		store:         session.NewStore(cfg.StateDir),
		transport:     NewTransport(in, out),
		logger:        logger,
		sessions:      map[string]*agent.Session{},
		sessionOps:    map[string]*promptHandle{},
		activePrompts: map[string]*promptHandle{},
		activeReloads: map[string]*promptHandle{},
		configOps:     map[string]*promptHandle{},
	}
}

// Bridge returns the active LAN T3 bridge if this Server acquired the singleton lock.
func (s *Server) Bridge() *bridge.Bridge {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bridge
}

// Run serves ACP JSON-RPC messages until EOF or ctx cancellation, then closes
// all active sessions so OpenAlbert can run post-session analysis.
func (s *Server) Run(ctx context.Context) error {
	if s.cfg.Bridge {
		b, acquired, err := bridge.New(s.cfg, s.client, s.logger)
		if err != nil {
			s.logger.Warn("failed to initialize LAN T3 bridge; continuing in interactive-only mode", "error", err)
		} else if !acquired {
			s.logger.Info("another openalbert instance holds the bridge lock; running in interactive-only mode")
		} else {
			s.mu.Lock()
			s.bridge = b
			s.mu.Unlock()
			b.Start(ctx)
		}
	}
	defer func() {
		s.closeAllSessions()
		s.closeBridge()
	}()
	return s.transport.ServeOrdered(ctx, s.prepareFrame, s.handle)
}

func (s *Server) closeBridge() {
	s.mu.Lock()
	b := s.bridge
	s.bridge = nil
	s.mu.Unlock()
	if b != nil {
		_ = b.Close()
	}
}

type sessionLoadParams struct {
	SessionID  string                   `json:"sessionId"`
	CWD        string                   `json:"cwd"`
	MCPServers []mcpclient.ServerConfig `json:"mcpServers"`
}

type setModelParams struct {
	SessionID string `json:"sessionId"`
	ModelID   string `json:"modelId"`
}

type setModeParams struct {
	SessionID string `json:"sessionId"`
	ModeID    string `json:"modeId"`
}

type setConfigOptionParams struct {
	SessionID string `json:"sessionId"`
	ConfigID  string `json:"configId"`
	Value     string `json:"value"`
}

type promptResource struct {
	URI          *string `json:"uri,omitempty"`
	MimeType     *string `json:"mimeType,omitempty"`
	Text         *string `json:"text,omitempty"`
	Blob         *string `json:"blob,omitempty"`
	resolvedMIME string
}

type promptContentBlock struct {
	Type         string          `json:"type"`
	Text         *string         `json:"text,omitempty"`
	Data         *string         `json:"data,omitempty"`
	MimeType     *string         `json:"mimeType,omitempty"`
	URI          *string         `json:"uri,omitempty"`
	Name         *string         `json:"name,omitempty"`
	Resource     *promptResource `json:"resource,omitempty"`
	resolvedMIME string
}

type promptParams struct {
	SessionID       string                `json:"sessionId"`
	Prompt          []*promptContentBlock `json:"prompt"`
	assembledText   string
	assembledImages []string
	assembledReplay []map[string]any
}

// registerSessionOp registers a prompt, reload, or config operation in strict
// arrival order under s.mu:
//   - Prompts cancel any prior prompt and wait on the previous sessionOp AND any preceding config op.
//   - Reloads cancel any prior prompt, wait on the previous sessionOp and any prior config op, and register in activeReloads.
//   - Config setters wait on any active reload or prior config op (never stalling on long-running prompts) and advance sessionOps when following a reload/config so subsequent prompts see the updated settings.
func (s *Server) registerSessionOp(ctx context.Context, sessionID string, kind sessionOpKind) *promptHandle {
	opCtx, cancel := context.WithCancel(ctx)
	h := &promptHandle{
		sessionID: sessionID,
		kind:      kind,
		ctx:       opCtx,
		cancel:    cancel,
		done:      make(chan struct{}),
	}
	s.mu.Lock()
	var prevToCancel *promptHandle
	switch kind {
	case opPrompt:
		prev := s.sessionOps[sessionID]
		h.prev = prev
		if cfgTail := s.configOps[sessionID]; cfgTail != nil && cfgTail != prev {
			h.waitReload = cfgTail
		}
		s.sessionOps[sessionID] = h
		s.activePrompts[sessionID] = h
		if prev != nil && prev.kind == opPrompt {
			prevToCancel = prev
		}
	case opLoad:
		prev := s.sessionOps[sessionID]
		h.prev = prev
		if cfgTail := s.configOps[sessionID]; cfgTail != nil && cfgTail != prev {
			h.waitReload = cfgTail
		}
		s.sessionOps[sessionID] = h
		s.activeReloads[sessionID] = h
		if prev != nil && prev.kind == opPrompt {
			prevToCancel = prev
		}
	case opConfig:
		if cfgTail := s.configOps[sessionID]; cfgTail != nil {
			h.prev = cfgTail
		}
		if rel := s.activeReloads[sessionID]; rel != nil && rel != h.prev {
			h.waitReload = rel
		}
		s.configOps[sessionID] = h
		if tail := s.sessionOps[sessionID]; tail != nil && tail.kind != opPrompt {
			s.sessionOps[sessionID] = h
		}
	}
	s.mu.Unlock()
	if prevToCancel != nil {
		prevToCancel.cancel()
	}
	return h
}

func (s *Server) finishSessionOp(h *promptHandle) {
	if h == nil {
		return
	}
	h.finishOnce.Do(func() {
		h.cancel()
		s.mu.Lock()
		if s.sessionOps[h.sessionID] == h {
			delete(s.sessionOps, h.sessionID)
		}
		if s.activePrompts[h.sessionID] == h {
			delete(s.activePrompts, h.sessionID)
		}
		if s.activeReloads[h.sessionID] == h {
			delete(s.activeReloads, h.sessionID)
		}
		if s.configOps[h.sessionID] == h {
			delete(s.configOps, h.sessionID)
		}
		s.mu.Unlock()
		close(h.done)
	})
}

func waitHandleDeps(ctx context.Context, h *promptHandle) error {
	if h == nil {
		return nil
	}
	if h.prev != nil {
		select {
		case <-h.prev.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if h.waitReload != nil {
		select {
		case <-h.waitReload.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func validModeID(mode string) bool {
	switch mode {
	case agent.ModeDefault, agent.ModeAutoAcceptEdits, agent.ModePlan, agent.ModeFullAccess:
		return true
	default:
		return false
	}
}

func parseSessionLoadParams(params json.RawMessage) (sessionLoadParams, error) {
	var req sessionLoadParams
	if err := json.Unmarshal(params, &req); err != nil || strings.TrimSpace(req.SessionID) == "" {
		return sessionLoadParams{}, fmt.Errorf("sessionId is required")
	}
	req.SessionID = strings.TrimSpace(req.SessionID)
	return req, nil
}

func parseSetModelParams(params json.RawMessage) (setModelParams, error) {
	var req setModelParams
	if err := json.Unmarshal(params, &req); err != nil || strings.TrimSpace(req.SessionID) == "" || strings.TrimSpace(req.ModelID) == "" {
		return setModelParams{}, fmt.Errorf("sessionId and modelId are required")
	}
	req.SessionID = strings.TrimSpace(req.SessionID)
	req.ModelID = strings.TrimSpace(req.ModelID)
	return req, nil
}

func parseSetModeParams(params json.RawMessage) (setModeParams, error) {
	var req setModeParams
	if err := json.Unmarshal(params, &req); err != nil || strings.TrimSpace(req.SessionID) == "" || !validModeID(req.ModeID) {
		return setModeParams{}, fmt.Errorf("sessionId and valid modeId are required")
	}
	req.SessionID = strings.TrimSpace(req.SessionID)
	return req, nil
}

func parseSetConfigOptionParams(params json.RawMessage) (setConfigOptionParams, error) {
	var req setConfigOptionParams
	if err := json.Unmarshal(params, &req); err != nil || strings.TrimSpace(req.SessionID) == "" || strings.TrimSpace(req.ConfigID) == "" || strings.TrimSpace(req.Value) == "" {
		return setConfigOptionParams{}, fmt.Errorf("sessionId, configId, and non-empty value are required")
	}
	req.SessionID = strings.TrimSpace(req.SessionID)
	req.ConfigID = strings.TrimSpace(req.ConfigID)
	req.Value = strings.TrimSpace(req.Value)
	if req.ConfigID == "mode" && !validModeID(req.Value) {
		return setConfigOptionParams{}, fmt.Errorf("invalid mode value %q", req.Value)
	}
	return req, nil
}

func normalizeSupportedImageMIME(mimeType string) (string, bool) {
	mime := strings.ToLower(strings.TrimSpace(strings.Split(mimeType, ";")[0]))
	switch mime {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
		return mime, true
	default:
		return "", false
	}
}

func decodeBase64Payload(b64 string) ([]byte, error) {
	trimmed := strings.TrimSpace(b64)
	if trimmed == "" {
		return nil, fmt.Errorf("empty base64 payload")
	}
	if strings.ContainsAny(trimmed, " \t") {
		return nil, fmt.Errorf("invalid base64 payload")
	}
	raw, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil {
		return nil, fmt.Errorf("invalid base64 payload: %w", err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty decoded payload")
	}
	return raw, nil
}

func detectImageMIME(raw []byte) (string, bool) {
	sniff := raw
	if len(sniff) > 512 {
		sniff = sniff[:512]
	}
	return normalizeSupportedImageMIME(http.DetectContentType(sniff))
}

func assemblePromptBlocks(blocks []*promptContentBlock) (string, []string, []map[string]any) {
	var textParts []string
	var images []string
	var userReplay []map[string]any
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if block.Text != nil && *block.Text != "" {
				textParts = append(textParts, *block.Text)
				userReplay = append(userReplay, map[string]any{
					"sessionUpdate": "user_message_chunk",
					"content":       map[string]any{"type": "text", "text": *block.Text},
				})
			}
		case "image":
			data := strings.TrimSpace(*block.Data)
			mime := block.resolvedMIME
			images = append(images, fmt.Sprintf("data:%s;base64,%s", mime, data))
			userReplay = append(userReplay, map[string]any{
				"sessionUpdate": "user_message_chunk",
				"content":       map[string]any{"type": "image", "mimeType": mime, "data": data},
			})
		case "resource":
			uri := strings.TrimSpace(*block.Resource.URI)
			if block.Resource.Text != nil {
				txt := fmt.Sprintf("[Resource %s]\n%s", uri, *block.Resource.Text)
				textParts = append(textParts, txt)
				userReplay = append(userReplay, map[string]any{
					"sessionUpdate": "user_message_chunk",
					"content":       map[string]any{"type": "text", "text": txt},
				})
			} else if block.Resource.Blob != nil {
				blob := strings.TrimSpace(*block.Resource.Blob)
				mime := block.Resource.resolvedMIME
				images = append(images, fmt.Sprintf("data:%s;base64,%s", mime, blob))
				textParts = append(textParts, fmt.Sprintf("[Resource %s]", uri))
				userReplay = append(userReplay, map[string]any{
					"sessionUpdate": "user_message_chunk",
					"content": map[string]any{
						"type": "resource",
						"resource": map[string]any{
							"uri":      uri,
							"mimeType": mime,
							"blob":     blob,
						},
					},
				})
			}
		case "resource_link":
			uri := strings.TrimSpace(*block.URI)
			name := strings.TrimSpace(*block.Name)
			txt := fmt.Sprintf("[Resource link: %s (%s)]", name, uri)
			textParts = append(textParts, txt)
			userReplay = append(userReplay, map[string]any{
				"sessionUpdate": "user_message_chunk",
				"content":       map[string]any{"type": "text", "text": txt},
			})
		}
	}
	return strings.Join(textParts, "\n"), images, userReplay
}

func parsePromptParams(params json.RawMessage) (promptParams, error) {
	var req promptParams
	if err := json.Unmarshal(params, &req); err != nil || strings.TrimSpace(req.SessionID) == "" || req.Prompt == nil {
		return promptParams{}, fmt.Errorf("invalid session/prompt params: sessionId and prompt array are required")
	}
	if len(req.Prompt) == 0 {
		return promptParams{}, fmt.Errorf("invalid session/prompt params: prompt array must not be empty")
	}
	for i, block := range req.Prompt {
		if block == nil {
			return promptParams{}, fmt.Errorf("invalid session/prompt params: prompt[%d] must be a content block object", i)
		}
		switch block.Type {
		case "text":
			if block.Text == nil {
				return promptParams{}, fmt.Errorf("invalid session/prompt params: prompt[%d] text is required", i)
			}
		case "image":
			if block.Data == nil || strings.TrimSpace(*block.Data) == "" {
				return promptParams{}, fmt.Errorf("invalid session/prompt params: prompt[%d] image data is required", i)
			}
			if _, err := decodeBase64Payload(*block.Data); err != nil {
				return promptParams{}, fmt.Errorf("invalid session/prompt params: prompt[%d] image data must be valid base64: %w", i, err)
			}
			if block.MimeType == nil || strings.TrimSpace(*block.MimeType) == "" {
				return promptParams{}, fmt.Errorf("invalid session/prompt params: prompt[%d] image mimeType is required", i)
			}
			norm, ok := normalizeSupportedImageMIME(*block.MimeType)
			if !ok {
				return promptParams{}, fmt.Errorf("invalid session/prompt params: prompt[%d] unsupported image mimeType %q", i, *block.MimeType)
			}
			block.resolvedMIME = norm
		case "resource":
			if block.Resource == nil {
				return promptParams{}, fmt.Errorf("invalid session/prompt params: prompt[%d] resource is required", i)
			}
			if block.Resource.URI == nil || strings.TrimSpace(*block.Resource.URI) == "" {
				return promptParams{}, fmt.Errorf("invalid session/prompt params: prompt[%d] resource.uri is required", i)
			}
			hasText := block.Resource.Text != nil
			hasBlob := block.Resource.Blob != nil
			if !hasText && !hasBlob {
				return promptParams{}, fmt.Errorf("invalid session/prompt params: prompt[%d] resource must contain text or blob", i)
			}
			if hasText && hasBlob {
				return promptParams{}, fmt.Errorf("invalid session/prompt params: prompt[%d] resource must not contain both text and blob", i)
			}
			if hasBlob {
				decoded, err := decodeBase64Payload(*block.Resource.Blob)
				if err != nil {
					return promptParams{}, fmt.Errorf("invalid session/prompt params: prompt[%d] resource.blob must be valid base64: %w", i, err)
				}
				var (
					norm string
					ok   bool
				)
				if block.Resource.MimeType != nil && strings.TrimSpace(*block.Resource.MimeType) != "" {
					norm, ok = normalizeSupportedImageMIME(*block.Resource.MimeType)
					if !ok {
						return promptParams{}, fmt.Errorf("invalid session/prompt params: prompt[%d] unsupported embedded resource blob mimeType %q", i, *block.Resource.MimeType)
					}
				} else {
					norm, ok = detectImageMIME(decoded)
					if !ok {
						return promptParams{}, fmt.Errorf("invalid session/prompt params: prompt[%d] unsupported embedded resource blob format", i)
					}
				}
				block.Resource.resolvedMIME = norm
			}
		case "resource_link":
			if block.URI == nil || strings.TrimSpace(*block.URI) == "" {
				return promptParams{}, fmt.Errorf("invalid session/prompt params: prompt[%d] resource_link uri is required", i)
			}
			if block.Name == nil || strings.TrimSpace(*block.Name) == "" {
				return promptParams{}, fmt.Errorf("invalid session/prompt params: prompt[%d] resource_link name is required", i)
			}
		default:
			return promptParams{}, fmt.Errorf("invalid session/prompt params: prompt[%d] unsupported type %q", i, block.Type)
		}
	}
	req.SessionID = strings.TrimSpace(req.SessionID)
	req.assembledText, req.assembledImages, req.assembledReplay = assemblePromptBlocks(req.Prompt)
	probeMsg := serverclient.Message{
		TurnID:  req.SessionID + "-t999999",
		Role:    "user",
		Content: req.assembledText,
		Images:  req.assembledImages,
	}
	if rawUser, _ := json.Marshal(probeMsg); len(rawUser) > agent.MaxSinglePromptBytes {
		return promptParams{}, fmt.Errorf("prompt exceeds maximum payload size (%d bytes > %d bytes); attach fewer or smaller images", len(rawUser), agent.MaxSinglePromptBytes)
	}
	return req, nil
}

// prepareFrame runs synchronously in the stdio read loop in exact receive order.
// It validates full request params BEFORE reserving a per-session queue entry so
// a malformed frame can never cancel a healthy prompt or disrupt reload/config ordering.
func (s *Server) prepareFrame(ctx context.Context, method string, params json.RawMessage) context.Context {
	switch method {
	case "session/prompt":
		if req, err := parsePromptParams(params); err == nil {
			h := s.registerSessionOp(ctx, req.SessionID, opPrompt)
			return context.WithValue(ctx, sessionOpKey{}, h)
		}
	case "session/load":
		if req, err := parseSessionLoadParams(params); err == nil {
			h := s.registerSessionOp(ctx, req.SessionID, opLoad)
			return context.WithValue(ctx, sessionOpKey{}, h)
		}
	case "session/set_model":
		if req, err := parseSetModelParams(params); err == nil {
			h := s.registerSessionOp(ctx, req.SessionID, opConfig)
			return context.WithValue(ctx, sessionOpKey{}, h)
		}
	case "session/set_mode":
		if req, err := parseSetModeParams(params); err == nil {
			h := s.registerSessionOp(ctx, req.SessionID, opConfig)
			return context.WithValue(ctx, sessionOpKey{}, h)
		}
	case "session/set_config_option":
		if req, err := parseSetConfigOptionParams(params); err == nil {
			h := s.registerSessionOp(ctx, req.SessionID, opConfig)
			return context.WithValue(ctx, sessionOpKey{}, h)
		}
	case "session/cancel":
		s.handleCancel(params)
	}
	return ctx
}

func (s *Server) closeAllSessions() {
	s.mu.Lock()
	active := make([]*agent.Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		active = append(active, sess)
	}
	for id, h := range s.activePrompts {
		h.cancel()
		delete(s.activePrompts, id)
	}
	for id, h := range s.sessionOps {
		h.cancel()
		delete(s.sessionOps, id)
	}
	for id, h := range s.activeReloads {
		h.cancel()
		delete(s.activeReloads, id)
	}
	for id, h := range s.configOps {
		h.cancel()
		delete(s.configOps, id)
	}
	s.sessions = map[string]*agent.Session{}
	b := s.bridge
	s.mu.Unlock()
	for _, sess := range active {
		if b != nil && sess.MCP != nil {
			b.DetachMCP(sess.MCP)
		}
		sess.Close(context.Background())
	}
}

func (s *Server) handle(ctx context.Context, id json.RawMessage, method string, params json.RawMessage) {
	// Guarantee any pre-registered session op is ALWAYS cleaned up on every exit
	// path, including invalid JSON params or early returns.
	if h, _ := ctx.Value(sessionOpKey{}).(*promptHandle); h != nil {
		defer s.finishSessionOp(h)
	}
	reply := func(result any, err error) {
		if len(id) == 0 {
			return
		}
		if err != nil {
			_ = s.transport.SendError(id, -32000, err.Error())
			return
		}
		_ = s.transport.SendResult(id, result)
	}

	switch method {
	case "initialize":
		reply(s.handleInitialize(params))
	case "authenticate":
		reply(map[string]any{}, nil)
	case "session/new":
		res, sess, err := s.handleSessionNew(ctx, params)
		reply(res, err)
		if err == nil && sess != nil {
			s.emitAvailableCommands(sess)
		}
	case "session/load":
		res, sess, err := s.handleSessionLoad(ctx, params)
		reply(res, err)
		if err == nil && sess != nil {
			s.emitAvailableCommands(sess)
		}
	case "session/set_model":
		reply(s.handleSetModel(ctx, params))
	case "session/set_mode":
		reply(s.handleSetMode(ctx, params))
	case "session/set_config_option":
		reply(s.handleSetConfigOption(ctx, params))
	case "session/prompt":
		reply(s.handlePrompt(ctx, params))
	case "session/cancel":
		// Already executed synchronously in prepareFrame in strict receive order.
	default:
		if len(id) > 0 {
			_ = s.transport.SendError(id, -32601, "method not found: "+method)
		}
	}
}

func (s *Server) handleInitialize(params json.RawMessage) (any, error) {
	var req struct {
		ProtocolVersion any `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &req)
	return map[string]any{
		"protocolVersion": SupportedProtocolVersion,
		"agentCapabilities": map[string]any{
			"loadSession": true,
			"promptCapabilities": map[string]any{
				"image":           true,
				"embeddedContext": true,
			},
			"mcpCapabilities": map[string]any{
				"http": true,
			},
		},
		"agentInfo": map[string]any{
			"name":    "openalbert",
			"title":   "OpenAlbert",
			"version": Version,
		},
		"authMethods": []any{},
	}, nil
}

func (s *Server) refreshModels(ctx context.Context) (string, []serverclient.CatalogModel, error) {
	resp, err := s.client.Models(ctx, false)
	if err != nil {
		return "", nil, err
	}
	def := s.cfg.DefaultModel
	if def == "" {
		def = resp.DefaultModel
	}
	if def == "" && len(resp.Models) > 0 {
		def = resp.Models[0].ID
	}
	s.mu.Lock()
	s.models = resp.Models
	s.defModel = def
	s.mu.Unlock()
	return def, resp.Models, nil
}

func (s *Server) buildSession(ctx context.Context, sessionID, cwd string, mcpConfigs []mcpclient.ServerConfig, existing *session.Record) (*agent.Session, error) {
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	absCWD, err := filepath.Abs(cwd)
	if err == nil {
		cwd = absCWD
	}
	defModel, _, err := s.refreshModels(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch OpenAlbert models: %w", err)
	}
	agentCtx, err := s.client.AgentContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch OpenAlbert agent context: %w", err)
	}

	reserved := map[string]bool{"skill_invoke": true}
	for _, t := range tools.Definitions() {
		reserved[t.Name] = true
	}
	for _, t := range agentCtx.Tools {
		reserved[t.Name] = true
	}
	mcpMgr, warnings := mcpclient.Connect(ctx, mcpConfigs, reserved)
	for _, w := range warnings {
		s.logger.Warn(w)
	}

	title := filepath.Base(cwd)
	if title == "" || title == "/" || title == "." {
		title = "Coding · " + sessionID
	}
	model := defModel
	mode := agent.ModeDefault
	effort := ""
	var messages []serverclient.Message
	var cmState map[string]json.RawMessage
	var pendingMsgs []serverclient.SyncMessage
	var pendingTraces []serverclient.Trace
	var replayUpdates []json.RawMessage
	turnCounter := 0
	generation := 1
	serverSessionID := sessionID
	if existing != nil {
		if existing.Title != "" {
			title = existing.Title
		}
		if existing.Model != "" {
			model = existing.Model
		}
		if existing.Mode != "" {
			mode = existing.Mode
		}
		effort = existing.ReasoningEffort
		var addedInterrupted []serverclient.Message
		messages, addedInterrupted = agent.RepairInterruptedToolCalls(existing.Messages)
		cmState = existing.CodemodeState
		turnCounter = existing.TurnCounter
		pendingMsgs = existing.PendingSyncMessages
		if s.cfg.SyncInspector {
			for _, m := range addedInterrupted {
				pendingMsgs = append(pendingMsgs, serverclient.SyncMessage{
					Key:     fmt.Sprintf("%s/tool/interrupted/%s", m.TurnID, m.ToolCallID),
					Message: m,
				})
			}
		}
		pendingTraces = existing.PendingSyncTraces
		replayUpdates = existing.ReplayUpdates
		if len(replayUpdates) == 0 && len(messages) > 0 {
			replayUpdates = synthesizeReplayUpdates(messages)
		}
		generation = existing.Generation
		if generation <= 0 {
			generation = 1
		}
		serverSessionID = existing.ServerSessionID
		if serverSessionID == "" {
			serverSessionID = sessionID
		}
		// Start a fresh server session generation whenever reopening a closed
		// (or previously analyzed) session so new turns are analyzed independently
		// under the same coding:<sessionID> conversation.
		if existing.Closed {
			generation++
			serverSessionID = fmt.Sprintf("%s-g%d", sessionID, generation)
		}
	}

	var sess *agent.Session
	remoteSkillReader := func(rctx context.Context, name string) (string, error) {
		args, _ := json.Marshal(map[string]string{"name": name})
		skillSessID := ""
		if s.cfg.SyncInspector {
			if sess != nil {
				skillSessID = sess.CurrentServerSessionID()
			} else {
				skillSessID = serverSessionID
			}
		}
		res, err := s.client.ExecuteTool(rctx, "skill_read", serverclient.ToolRequest{
			SessionID: skillSessID,
			Arguments: args,
		})
		if err != nil {
			return "", err
		}
		if res.Failed {
			return "", fmt.Errorf("server skill_read failed: %s", string(res.Output))
		}
		var body struct {
			Content string `json:"content"`
		}
		if json.Unmarshal(res.Output, &body) == nil && body.Content != "" {
			return body.Content, nil
		}
		return string(res.Output), nil
	}
	skillReg := skills.Discover(cwd, "", agentCtx.Skills, remoteSkillReader)

	if !s.supportsReasoningEffort(model, effort) {
		effort = ""
	}

	sess = &agent.Session{
		ID:                  sessionID,
		ServerSessionID:     serverSessionID,
		Generation:          generation,
		Closed:              false,
		CWD:                 cwd,
		Title:               title,
		Model:               model,
		Mode:                mode,
		ReasoningEffort:     effort,
		Messages:            messages,
		CodemodeState:       cmState,
		TurnCounter:         turnCounter,
		ApprovedTools:       map[string]bool{},
		PendingSyncMessages: pendingMsgs,
		PendingSyncTraces:   pendingTraces,
		ReplayUpdates:       replayUpdates,
		Client:              s.client,
		Store:               s.store,
		LocalTools:          &tools.Local{CWD: cwd},
		Skills:              skillReg,
		MCP:                 mcpMgr,
		ServerContext:       agentCtx,
		SyncInspector:       s.cfg.SyncInspector,
		MaxSteps:            s.cfg.MaxSteps,
	}
	// Persist locally BEFORE creating any remote Inspector session so a local
	// persistence failure never strands an open remote session.
	if err := sess.Save(); err != nil {
		if sess.MCP != nil {
			sess.MCP.Close()
		}
		return nil, fmt.Errorf("persist session %q: %w", sessionID, err)
	}
	if s.cfg.SyncInspector {
		for attempt := 0; attempt < 5; attempt++ {
			err := s.client.CreateCodingSession(ctx, serverSessionID, "coding:"+sessionID, title, cwd)
			if err == nil {
				break
			}
			// If the remote server already closed this generation (e.g. a previous
			// close succeeded on the server but the response was lost), advance to
			// a fresh generation under the same coding:<sessionID> conversation.
			var apiErr *serverclient.APIError
			if errors.As(err, &apiErr) && apiErr.Status == 409 && apiErr.Code == "session_closed" {
				generation++
				serverSessionID = fmt.Sprintf("%s-g%d", sessionID, generation)
				sess.Generation = generation
				sess.ServerSessionID = serverSessionID
				for i := range sess.PendingSyncTraces {
					sess.PendingSyncTraces[i].SessionID = serverSessionID
				}
				_ = sess.Save()
				continue
			}
			s.logger.Warn("failed to create remote Inspector session", "session_id", serverSessionID, "error", err)
			break
		}
	}
	s.mu.Lock()
	prev := s.sessions[sessionID]
	s.sessions[sessionID] = sess
	b := s.bridge
	s.mu.Unlock()
	if b != nil && bridge.HasT3Tools(sess.MCP) {
		b.AttachMCP(sess.MCP)
	}
	if prev != nil && prev.MCP != nil {
		if b != nil {
			b.DetachMCP(prev.MCP)
		}
		prev.MCP.Close()
	}
	return sess, nil
}

func (s *Server) modelEfforts(modelID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.models {
		if m.ID == modelID {
			return append([]string(nil), m.SupportedReasoningEfforts...)
		}
	}
	return nil
}

func formatReasoningEffortName(effort string) string {
	switch effort {
	case "xhigh":
		return "Extra High"
	default:
		if effort == "" {
			return ""
		}
		return strings.ToUpper(effort[:1]) + effort[1:]
	}
}

func (s *Server) supportsReasoningEffort(modelID, effort string) bool {
	if effort == "" || effort == "default" {
		return true
	}
	for _, allowed := range s.modelEfforts(modelID) {
		if allowed == effort {
			return true
		}
	}
	return false
}

func (s *Server) sessionMetadata(sess *agent.Session) map[string]any {
	s.mu.Lock()
	models := append([]serverclient.CatalogModel(nil), s.models...)
	s.mu.Unlock()
	curModel, curMode, curEffort := sess.SettingsSnapshot()

	availModels := make([]map[string]any, 0, len(models))
	modelSelectOpts := make([]map[string]any, 0, len(models))
	for _, m := range models {
		name := m.DisplayName
		if name == "" {
			name = m.ID
		}
		availModels = append(availModels, map[string]any{
			"modelId":     m.ID,
			"name":        name,
			"description": fmt.Sprintf("%s (%dk context)", m.Provider, m.ContextWindowTokens/1000),
		})
		modelSelectOpts = append(modelSelectOpts, map[string]any{
			"value":       m.ID,
			"name":        name,
			"description": m.ID,
		})
	}
	modes := []map[string]any{
		{"id": agent.ModeDefault, "name": "Default", "description": "Ask before file edits and shell commands"},
		{"id": agent.ModeAutoAcceptEdits, "name": "Auto-accept edits", "description": "Auto-approve file edits; ask before shell commands"},
		{"id": agent.ModePlan, "name": "Plan", "description": "Read-only planning mode"},
		{"id": agent.ModeFullAccess, "name": "Full access", "description": "Auto-approve all local and remote tool calls"},
	}
	modeOpts := make([]map[string]any, 0, len(modes))
	for _, m := range modes {
		modeOpts = append(modeOpts, map[string]any{"value": m["id"], "name": m["name"], "description": m["description"]})
	}
	configOptions := []map[string]any{
		{
			"id":           "model",
			"name":         "Model",
			"category":     "model",
			"type":         "select",
			"currentValue": curModel,
			"options":      modelSelectOpts,
		},
		{
			"id":           "mode",
			"name":         "Mode",
			"category":     "mode",
			"type":         "select",
			"currentValue": curMode,
			"options":      modeOpts,
		},
	}
	if supported := s.modelEfforts(curModel); len(supported) > 0 {
		effortVal := curEffort
		if effortVal == "" {
			effortVal = "default"
		}
		effortOpts := []map[string]any{{"value": "default", "name": "Default"}}
		for _, e := range supported {
			effortOpts = append(effortOpts, map[string]any{"value": e, "name": formatReasoningEffortName(e)})
		}
		configOptions = append(configOptions, map[string]any{
			"id":           "reasoning_effort",
			"name":         "Reasoning Effort",
			"category":     "thought_level",
			"type":         "select",
			"currentValue": effortVal,
			"options":      effortOpts,
		})
	}
	return map[string]any{
		"sessionId": sess.ID,
		"models": map[string]any{
			"currentModelId":  curModel,
			"availableModels": availModels,
		},
		"modes": map[string]any{
			"currentModeId":  curMode,
			"availableModes": modes,
		},
		"configOptions": configOptions,
	}
}

func (s *Server) handleSessionNew(ctx context.Context, params json.RawMessage) (any, *agent.Session, error) {
	var req struct {
		CWD        string                   `json:"cwd"`
		MCPServers []mcpclient.ServerConfig `json:"mcpServers"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, nil, fmt.Errorf("invalid session/new params: %w", err)
	}
	sessionID := "oa-" + uuid.NewString()
	sess, err := s.buildSession(ctx, sessionID, req.CWD, req.MCPServers, nil)
	if err != nil {
		return nil, nil, err
	}
	return s.sessionMetadata(sess), sess, nil
}

func (s *Server) handleSessionLoad(ctx context.Context, params json.RawMessage) (any, *agent.Session, error) {
	handle, _ := ctx.Value(sessionOpKey{}).(*promptHandle)
	if handle != nil {
		defer s.finishSessionOp(handle)
	}
	req, err := parseSessionLoadParams(params)
	if err != nil {
		return nil, nil, err
	}
	if handle == nil {
		handle = s.registerSessionOp(ctx, req.SessionID, opLoad)
		defer s.finishSessionOp(handle)
	}

	// Wait for any earlier prompt, reload, or settings setter to finish BEFORE
	// reading persisted state from disk. Incoming prompts/setters arriving during
	// reload wait on handle.done and run against the replacement session.
	// Note: do NOT close the previous MCP manager until buildSession succeeds.
	if err := waitHandleDeps(ctx, handle); err != nil {
		return nil, nil, err
	}

	rec, ok, err := s.store.Load(req.SessionID)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, nil, fmt.Errorf("session %q not found", req.SessionID)
	}
	cwd := req.CWD
	if cwd == "" {
		cwd = rec.CWD
	}
	sess, err := s.buildSession(ctx, req.SessionID, cwd, req.MCPServers, &rec)
	if err != nil {
		return nil, nil, err
	}
	// Replay persisted ACP conversation updates (including any migrated legacy prefix).
	for _, rawUpdate := range sess.ReplayUpdates {
		_ = s.transport.Notify("session/update", map[string]any{
			"sessionId": sess.ID,
			"update":    rawUpdate,
		})
	}
	return s.sessionMetadata(sess), sess, nil
}

func synthesizeReplayUpdates(messages []serverclient.Message) []json.RawMessage {
	var out []json.RawMessage
	emit := func(u map[string]any) {
		if raw, err := json.Marshal(u); err == nil {
			out = append(out, raw)
		}
	}
	for _, msg := range messages {
		if msg.ParentToolCallID != "" {
			continue
		}
		switch msg.Role {
		case "user":
			if msg.Content != "" {
				emit(map[string]any{
					"sessionUpdate": "user_message_chunk",
					"content":       map[string]any{"type": "text", "text": msg.Content},
				})
			}
			for _, dataURL := range msg.Images {
				mime, b64 := parseDataURL(dataURL)
				if b64 != "" {
					emit(map[string]any{
						"sessionUpdate": "user_message_chunk",
						"content":       map[string]any{"type": "image", "mimeType": mime, "data": b64},
					})
				}
			}
		case "assistant":
			if msg.Reasoning != "" {
				emit(map[string]any{
					"sessionUpdate": "agent_thought_chunk",
					"content":       map[string]any{"type": "text", "text": msg.Reasoning},
				})
			}
			if msg.Content != "" {
				emit(map[string]any{
					"sessionUpdate": "agent_message_chunk",
					"content":       map[string]any{"type": "text", "text": msg.Content},
				})
			}
			for _, tc := range msg.ToolCalls {
				emit(map[string]any{
					"sessionUpdate": "tool_call",
					"toolCallId":    tc.ID,
					"title":         tc.Name,
					"kind":          tools.ACPKind(tc.Name),
					"status":        "completed",
					"rawInput":      tc.Arguments,
				})
			}
		case "tool":
			if msg.ToolCallID != "" {
				emit(map[string]any{
					"sessionUpdate": "tool_call_update",
					"toolCallId":    msg.ToolCallID,
					"status":        "completed",
					"content": []map[string]any{{
						"type":    "content",
						"content": map[string]any{"type": "text", "text": msg.Content},
					}},
				})
			}
		}
	}
	return out
}

func parseDataURL(u string) (mimeType, b64 string) {
	if !strings.HasPrefix(u, "data:") {
		return "", ""
	}
	rest := strings.TrimPrefix(u, "data:")
	meta, data, ok := strings.Cut(rest, ",")
	if !ok {
		return "", ""
	}
	mime := strings.TrimSuffix(meta, ";base64")
	if mime == "" {
		mime = "image/png"
	}
	return mime, data
}

func (s *Server) emitAvailableCommands(sess *agent.Session) {
	if sess.Skills == nil {
		return
	}
	list := sess.Skills.List()
	cmds := make([]map[string]any, 0, len(list))
	for _, sk := range list {
		cmds = append(cmds, map[string]any{
			"name":        sk.Name,
			"description": sk.Description,
			"input":       map[string]string{"hint": "Optional skill arguments"},
		})
	}
	_ = s.transport.Notify("session/update", map[string]any{
		"sessionId": sess.ID,
		"update": map[string]any{
			"sessionUpdate":     "available_commands_update",
			"availableCommands": cmds,
		},
	})
}

func (s *Server) getSession(id string) (*agent.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return nil, fmt.Errorf("unknown session %q", id)
	}
	return sess, nil
}

func (s *Server) handleSetModel(ctx context.Context, params json.RawMessage) (any, error) {
	handle, _ := ctx.Value(sessionOpKey{}).(*promptHandle)
	if handle != nil {
		defer s.finishSessionOp(handle)
	}
	req, err := parseSetModelParams(params)
	if err != nil {
		return nil, err
	}
	if handle == nil {
		handle = s.registerSessionOp(ctx, req.SessionID, opConfig)
		defer s.finishSessionOp(handle)
	}
	if err := waitHandleDeps(ctx, handle); err != nil {
		return nil, err
	}
	sess, err := s.getSession(req.SessionID)
	if err != nil {
		return nil, err
	}
	if err := sess.SetModelAndAdjustEffort(req.ModelID, s.supportsReasoningEffort); err != nil {
		return nil, fmt.Errorf("persist session model: %w", err)
	}
	meta := s.sessionMetadata(sess)
	_ = s.transport.Notify("session/update", map[string]any{
		"sessionId": sess.ID,
		"update": map[string]any{
			"sessionUpdate": "config_option_update",
			"configOptions": meta["configOptions"],
		},
	})
	return map[string]any{}, nil
}

func (s *Server) handleSetMode(ctx context.Context, params json.RawMessage) (any, error) {
	handle, _ := ctx.Value(sessionOpKey{}).(*promptHandle)
	if handle != nil {
		defer s.finishSessionOp(handle)
	}
	req, err := parseSetModeParams(params)
	if err != nil {
		return nil, err
	}
	if handle == nil {
		handle = s.registerSessionOp(ctx, req.SessionID, opConfig)
		defer s.finishSessionOp(handle)
	}
	if err := waitHandleDeps(ctx, handle); err != nil {
		return nil, err
	}
	sess, err := s.getSession(req.SessionID)
	if err != nil {
		return nil, err
	}
	if err := sess.SetMode(req.ModeID); err != nil {
		return nil, fmt.Errorf("persist session mode: %w", err)
	}
	_ = s.transport.Notify("session/update", map[string]any{
		"sessionId": sess.ID,
		"update": map[string]any{
			"sessionUpdate": "current_mode_update",
			"currentModeId": req.ModeID,
		},
	})
	return map[string]any{}, nil
}

func (s *Server) handleSetConfigOption(ctx context.Context, params json.RawMessage) (any, error) {
	handle, _ := ctx.Value(sessionOpKey{}).(*promptHandle)
	if handle != nil {
		defer s.finishSessionOp(handle)
	}
	req, err := parseSetConfigOptionParams(params)
	if err != nil {
		return nil, err
	}
	if handle == nil {
		handle = s.registerSessionOp(ctx, req.SessionID, opConfig)
		defer s.finishSessionOp(handle)
	}
	if err := waitHandleDeps(ctx, handle); err != nil {
		return nil, err
	}
	sess, err := s.getSession(req.SessionID)
	if err != nil {
		return nil, err
	}
	switch req.ConfigID {
	case "model":
		if err := sess.SetModelAndAdjustEffort(req.Value, s.supportsReasoningEffort); err != nil {
			return nil, fmt.Errorf("persist session model: %w", err)
		}
	case "mode":
		if err := sess.SetMode(req.Value); err != nil {
			return nil, fmt.Errorf("persist session mode: %w", err)
		}
		_ = s.transport.Notify("session/update", map[string]any{
			"sessionId": sess.ID,
			"update": map[string]any{
				"sessionUpdate": "current_mode_update",
				"currentModeId": req.Value,
			},
		})
	case "reasoning_effort":
		model, ok, saveErr := sess.SetReasoningEffortValidated(req.Value, s.supportsReasoningEffort)
		if !ok {
			return nil, fmt.Errorf("reasoning_effort %q is not supported by model %q", req.Value, model)
		}
		if saveErr != nil {
			return nil, fmt.Errorf("persist session reasoning_effort: %w", saveErr)
		}
	default:
		return nil, fmt.Errorf("unsupported configId %q", req.ConfigID)
	}
	meta := s.sessionMetadata(sess)
	return map[string]any{"configOptions": meta["configOptions"]}, nil
}

func (s *Server) handleCancel(params json.RawMessage) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(params, &req) != nil || req.SessionID == "" {
		return
	}
	s.mu.Lock()
	h := s.activePrompts[req.SessionID]
	s.mu.Unlock()
	if h != nil {
		h.cancel()
	}
}

func (s *Server) handlePrompt(ctx context.Context, params json.RawMessage) (any, error) {
	handle, _ := ctx.Value(sessionOpKey{}).(*promptHandle)
	if handle != nil {
		defer s.finishSessionOp(handle)
	}
	req, err := parsePromptParams(params)
	if err != nil {
		return nil, err
	}

	// Use the operation handle registered synchronously in receive order (or
	// register one now for direct in-process test calls). Wait for any preceding
	// prompt, reload, or post-reload config update BEFORE resolving the live session.
	if handle == nil {
		handle = s.registerSessionOp(ctx, req.SessionID, opPrompt)
		defer s.finishSessionOp(handle)
	}

	if err := waitHandleDeps(ctx, handle); err != nil {
		return nil, err
	}
	if handle.ctx.Err() != nil {
		return map[string]any{"stopReason": "cancelled"}, nil
	}
	sess, err := s.getSession(req.SessionID)
	if err != nil {
		return nil, err
	}
	if s.isBridgeTickPrompt(req) {
		return s.handleBridgeTick(handle.ctx, sess)
	}
	cb := &acpCallbacks{
		sessionID:      sess.ID,
		sess:           sess,
		transport:      s.transport,
		pendingUserBuf: req.assembledReplay,
	}
	stopReason, err := sess.RunTurn(handle.ctx, req.assembledText, req.assembledImages, cb)
	cb.FlushReplay()
	if err != nil {
		return nil, err
	}
	return map[string]any{"stopReason": stopReason}, nil
}

func (s *Server) isBridgeTickPrompt(req promptParams) bool {
	if !s.cfg.Bridge || len(req.assembledImages) > 0 {
		return false
	}
	tick := s.cfg.BridgeTickPrompt
	if tick == "" {
		tick = config.DefaultBridgeTickPrompt
	}
	return matchesBridgeTick(req.assembledText, tick)
}

// matchesBridgeTick returns true when raw is either the exact tick sentinel or
// a T3 Code ACP prompt envelope whose extracted user request payload is the
// exact tick sentinel (optionally preceded by a T3 Code system metadata line
// such as "[T3 Code: ...]" or "[Scheduled ...]").
func matchesBridgeTick(raw, tick string) bool {
	if tick == "" {
		return false
	}
	if raw == tick {
		return true
	}
	candidate := raw
	fromEnvelope := false
	if inner, ok := extractSingleUserRequestEnvelope(raw); ok {
		candidate = strings.TrimSpace(inner)
		fromEnvelope = true
	}
	stripped, hadMetaPrefix := stripT3SystemMetadataPrefix(candidate)
	if !fromEnvelope && !hadMetaPrefix {
		return false
	}
	return stripped == tick
}

func extractSingleUserRequestEnvelope(raw string) (string, bool) {
	lower := strings.ToLower(raw)
	const openTag = "<user_request>"
	const closeTag = "</user_request>"
	start := strings.Index(lower, openTag)
	if start < 0 {
		return "", false
	}
	// Reject ambiguous prompts with multiple <user_request> blocks.
	if strings.Contains(lower[start+len(openTag):], openTag) {
		return "", false
	}
	endRel := strings.Index(lower[start+len(openTag):], closeTag)
	if endRel < 0 {
		return "", false
	}
	innerStart := start + len(openTag)
	innerEnd := innerStart + endRel
	return raw[innerStart:innerEnd], true
}

func stripT3SystemMetadataPrefix(text string) (string, bool) {
	strippedAny := false
	cur := text
	for {
		trimmed := strings.TrimLeft(cur, "\r\n")
		if !(strings.HasPrefix(trimmed, "[T3 Code:") || strings.HasPrefix(trimmed, "[Scheduled")) {
			break
		}
		nl := strings.IndexByte(trimmed, '\n')
		if nl < 0 {
			break
		}
		line := strings.TrimSpace(trimmed[:nl])
		if !strings.HasSuffix(line, "]") {
			break
		}
		cur = strings.TrimLeft(trimmed[nl+1:], "\r\n")
		strippedAny = true
	}
	if strippedAny {
		return strings.TrimSpace(cur), true
	}
	return text, false
}

func (s *Server) handleBridgeTick(ctx context.Context, sess *agent.Session) (any, error) {
	s.mu.Lock()
	b := s.bridge
	s.mu.Unlock()
	replyText := "bridge ok"
	if b != nil {
		if err := b.Tick(ctx, sess.MCP); err != nil {
			s.logger.Warn("bridge tick encountered error", "session_id", sess.ID, "error", err)
		}
	} else {
		replyText = "bridge ok (standby)"
	}
	if sess != nil && sess.MCP != nil && sess.MCP.HasRemoteTool("t3_thread_organize") {
		if _, err := sess.MCP.CallRemote(ctx, "t3_thread_organize", map[string]any{
			"action":       "snooze",
			"snoozedUntil": "2099-01-01T00:00:00Z",
		}); err != nil {
			s.logger.Debug("bridge tick auto-snooze failed", "session_id", sess.ID, "error", err)
		}
	}
	_ = s.transport.Notify("session/update", map[string]any{
		"sessionId": sess.ID,
		"update": map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content":       map[string]any{"type": "text", "text": replyText},
		},
	})
	return map[string]any{"stopReason": "end_turn"}, nil
}

type acpCallbacks struct {
	mu             sync.Mutex
	sessionID      string
	sess           *agent.Session
	transport      *Transport
	pendingUserBuf []map[string]any
	thoughtBuf     strings.Builder
	messageBuf     strings.Builder
}

func (c *acpCallbacks) flushUserLocked() {
	if len(c.pendingUserBuf) == 0 || c.sess == nil {
		return
	}
	for _, u := range c.pendingUserBuf {
		c.sess.AppendReplayUpdate(u)
	}
	c.pendingUserBuf = nil
}

func (c *acpCallbacks) UserAccepted() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.flushUserLocked()
}

func (c *acpCallbacks) flushTextLocked() {
	if c.sess == nil {
		return
	}
	if c.thoughtBuf.Len() > 0 {
		c.sess.AppendReplayUpdate(map[string]any{
			"sessionUpdate": "agent_thought_chunk",
			"content":       map[string]any{"type": "text", "text": c.thoughtBuf.String()},
		})
		c.thoughtBuf.Reset()
	}
	if c.messageBuf.Len() > 0 {
		c.sess.AppendReplayUpdate(map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content":       map[string]any{"type": "text", "text": c.messageBuf.String()},
		})
		c.messageBuf.Reset()
	}
}

func (c *acpCallbacks) FlushReplay() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.flushTextLocked()
}

func (c *acpCallbacks) SendThought(delta string) {
	if delta == "" {
		return
	}
	c.mu.Lock()
	c.flushUserLocked()
	if c.messageBuf.Len() > 0 {
		c.flushTextLocked()
	}
	c.thoughtBuf.WriteString(delta)
	c.mu.Unlock()
	_ = c.transport.Notify("session/update", map[string]any{
		"sessionId": c.sessionID,
		"update": map[string]any{
			"sessionUpdate": "agent_thought_chunk",
			"content":       map[string]any{"type": "text", "text": delta},
		},
	})
}

func (c *acpCallbacks) SendMessage(delta string) {
	if delta == "" {
		return
	}
	c.mu.Lock()
	c.flushUserLocked()
	if c.thoughtBuf.Len() > 0 {
		c.flushTextLocked()
	}
	c.messageBuf.WriteString(delta)
	c.mu.Unlock()
	_ = c.transport.Notify("session/update", map[string]any{
		"sessionId": c.sessionID,
		"update": map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content":       map[string]any{"type": "text", "text": delta},
		},
	})
}

func formatLocations(paths []string) []map[string]any {
	if len(paths) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(paths))
	for _, p := range paths {
		out = append(out, map[string]any{"path": p})
	}
	return out
}

func (c *acpCallbacks) ToolStart(callID, title, kind string, rawInput json.RawMessage, locations []string) {
	update := map[string]any{
		"sessionUpdate": "tool_call",
		"toolCallId":    callID,
		"title":         title,
		"kind":          kind,
		"status":        "in_progress",
		"rawInput":      rawInput,
	}
	if locs := formatLocations(locations); len(locs) > 0 {
		update["locations"] = locs
	}
	c.mu.Lock()
	c.flushUserLocked()
	c.flushTextLocked()
	if c.sess != nil {
		c.sess.AppendReplayUpdate(update)
	}
	c.mu.Unlock()
	_ = c.transport.Notify("session/update", map[string]any{
		"sessionId": c.sessionID,
		"update":    update,
	})
}

func (c *acpCallbacks) ToolProgress(callID, text string) {
	if text == "" {
		return
	}
	_ = c.transport.Notify("session/update", map[string]any{
		"sessionId": c.sessionID,
		"update": map[string]any{
			"sessionUpdate": "tool_call_update",
			"toolCallId":    callID,
			"status":        "in_progress",
			"content": []map[string]any{{
				"type":    "content",
				"content": map[string]any{"type": "text", "text": text},
			}},
		},
	})
}

func (c *acpCallbacks) ToolFinish(callID, status string, diff *tools.FileDiff, text string, rawOutput json.RawMessage) {
	var content []map[string]any
	if diff != nil {
		content = append(content, map[string]any{
			"type":    "diff",
			"path":    diff.Path,
			"oldText": diff.OldText,
			"newText": diff.NewText,
		})
	}
	if text != "" {
		content = append(content, map[string]any{
			"type":    "content",
			"content": map[string]any{"type": "text", "text": text},
		})
	}
	update := map[string]any{
		"sessionUpdate": "tool_call_update",
		"toolCallId":    callID,
		"status":        status,
		"rawOutput":     rawOutput,
	}
	if len(content) > 0 {
		update["content"] = content
	}
	c.mu.Lock()
	c.flushUserLocked()
	c.flushTextLocked()
	if c.sess != nil {
		c.sess.AppendReplayUpdate(update)
	}
	c.mu.Unlock()
	_ = c.transport.Notify("session/update", map[string]any{
		"sessionId": c.sessionID,
		"update":    update,
	})
}

func (c *acpCallbacks) RequestPermission(ctx context.Context, callID, title, kind string, rawInput json.RawMessage, locations []string) (string, error) {
	toolCall := map[string]any{
		"toolCallId": callID,
		"title":      title,
		"kind":       kind,
		"status":     "pending",
		"rawInput":   rawInput,
	}
	if locs := formatLocations(locations); len(locs) > 0 {
		toolCall["locations"] = locs
	}
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	var resp struct {
		Outcome struct {
			Outcome  string `json:"outcome"`
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	}
	err := c.transport.Call(reqCtx, "session/request_permission", map[string]any{
		"sessionId": c.sessionID,
		"toolCall":  toolCall,
		"options": []map[string]any{
			{"optionId": "allow_once", "name": "Allow once", "kind": "allow_once"},
			{"optionId": "allow_always", "name": "Allow always (this session)", "kind": "allow_always"},
			{"optionId": "reject_once", "name": "Reject", "kind": "reject_once"},
		},
	}, &resp)
	if err != nil {
		return "reject_once", err
	}
	if resp.Outcome.Outcome != "selected" {
		return "reject_once", nil
	}
	return resp.Outcome.OptionID, nil
}
