package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/mishankov/OpenAlbert/apps/cli/internal/config"
	"github.com/mishankov/OpenAlbert/apps/cli/internal/serverclient"
)

const defaultCLIWatchInterval = 2 * time.Second

// CLIRunner executes the official `t3` CLI binary. Overridable in tests so
// unit tests run identically on macOS, Linux, and Windows.
type CLIRunner func(ctx context.Context, bin string, args []string, stdin string) ([]byte, error)

// CLIBridge runs a standalone, cross-platform (macOS/Windows/Linux) 100%-LAN
// T3 Code bridge backed by the official `t3` CLI (`t3 thread start/send/show`).
// It needs no bridge thread or scheduled ticks in the T3 Code UI and executes
// Pi commands in near real-time over SSE (~250ms).
type CLIBridge struct {
	cfg       config.Config
	client    *serverclient.Client
	logger    *slog.Logger
	lock      *FileLock
	t3Bin     string
	t3BaseDir string
	runCLI    CLIRunner

	WatchInterval      time.Duration
	HeartbeatInterval  time.Duration
	ReconnectBaseDelay time.Duration
	ReconnectMaxDelay  time.Duration

	mu             sync.Mutex
	envID          string
	nextTrackGen   uint64
	activeThreads  map[string]*trackedThread
	threadPollMu   map[string]*sync.Mutex
	inFlightCmds   map[string]bool
	completedCmds  map[string]time.Time
	pendingResults map[string]serverclient.T3CommandResultRequest
	closeOnce      sync.Once
}

// ResolveT3Bin locates the official `t3` CLI binary across macOS, Linux, and Windows.
func ResolveT3Bin(explicit string) (string, error) {
	if p := strings.TrimSpace(explicit); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		if lp, err := exec.LookPath(p); err == nil {
			return lp, nil
		}
		return "", fmt.Errorf("configured t3 binary not found: %s", p)
	}
	if lp, err := exec.LookPath("t3"); err == nil {
		return lp, nil
	}
	home, _ := os.UserHomeDir()
	if home != "" {
		candidates := []string{filepath.Join(home, ".t3", "bin", "t3")}
		if runtime.GOOS == "windows" {
			candidates = []string{
				filepath.Join(home, ".t3", "bin", "t3.exe"),
				filepath.Join(home, ".t3", "bin", "t3.cmd"),
				filepath.Join(home, ".t3", "bin", "t3"),
			}
		}
		for _, c := range candidates {
			if st, err := os.Stat(c); err == nil && !st.IsDir() {
				return c, nil
			}
		}
	}
	return "", errors.New("t3 CLI binary not found (set t3_bin in config.toml or OPENALBERT_T3_BIN, or install `t3`)")
}

// ResolveT3BaseDir resolves the T3 Code data directory (defaults to ~/.t3).
func ResolveT3BaseDir(explicit string) string {
	if p := strings.TrimSpace(explicit); p != "" {
		return p
	}
	if env := strings.TrimSpace(os.Getenv("T3CODE_HOME")); env != "" {
		return env
	}
	home, _ := os.UserHomeDir()
	if home != "" {
		return filepath.Join(home, ".t3")
	}
	return ""
}

// ReadEnvironmentID reads the local T3 Code environment ID from cfg or <baseDir>/userdata/environment-id.
func ReadEnvironmentID(explicit, baseDir string) (string, error) {
	if id := strings.TrimSpace(explicit); id != "" {
		return id, nil
	}
	if baseDir != "" {
		p := filepath.Join(baseDir, "userdata", "environment-id")
		if b, err := os.ReadFile(p); err == nil {
			if id := strings.TrimSpace(string(b)); id != "" {
				return id, nil
			}
		}
	}
	return "", errors.New("T3 environment ID not found (set environment_id in config.toml or ensure ~/.t3/userdata/environment-id exists)")
}

func defaultCLIRunner(ctx context.Context, bin string, args []string, stdin string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		if detail != "" {
			return nil, fmt.Errorf("t3 %s failed: %s", strings.Join(args[:min(2, len(args))], " "), detail)
		}
		return nil, fmt.Errorf("t3 %s failed: %w", strings.Join(args[:min(2, len(args))], " "), err)
	}
	return stdout.Bytes(), nil
}

// NewCLIBridge acquires the singleton bridge lock and constructs a standalone CLI bridge.
func NewCLIBridge(cfg config.Config, client *serverclient.Client, logger *slog.Logger) (*CLIBridge, error) {
	return newCLIBridgeWithRunner(cfg, client, logger, nil)
}

func newCLIBridgeWithRunner(cfg config.Config, client *serverclient.Client, logger *slog.Logger, runner CLIRunner) (*CLIBridge, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	baseDir := ResolveT3BaseDir(cfg.T3BaseDir)
	envID, err := ReadEnvironmentID(cfg.EnvironmentID, baseDir)
	if err != nil {
		return nil, err
	}
	var t3Bin string
	if runner == nil {
		t3Bin, err = ResolveT3Bin(cfg.T3Bin)
		if err != nil {
			return nil, err
		}
		runner = defaultCLIRunner
	} else {
		t3Bin = firstNonEmpty(cfg.T3Bin, "t3")
	}

	lock, acquired, err := AcquireLock(cfg.BridgeLockPath())
	if err != nil {
		return nil, err
	}
	if !acquired {
		return nil, fmt.Errorf("another OpenAlbert bridge instance already holds %s", cfg.BridgeLockPath())
	}

	return &CLIBridge{
		cfg:                cfg,
		client:             client,
		logger:             logger,
		lock:               lock,
		t3Bin:              t3Bin,
		t3BaseDir:          baseDir,
		runCLI:             runner,
		WatchInterval:      defaultCLIWatchInterval,
		HeartbeatInterval:  defaultHeartbeatInterval,
		ReconnectBaseDelay: defaultReconnectBaseDelay,
		ReconnectMaxDelay:  defaultReconnectMaxDelay,
		envID:              envID,
		activeThreads:      map[string]*trackedThread{},
		threadPollMu:       map[string]*sync.Mutex{},
		inFlightCmds:       map[string]bool{},
		completedCmds:      map[string]time.Time{},
		pendingResults:     map[string]serverclient.T3CommandResultRequest{},
	}, nil
}

// Close releases the singleton lock.
func (b *CLIBridge) Close() error {
	if b == nil {
		return nil
	}
	var err error
	b.closeOnce.Do(func() {
		if b.lock != nil {
			err = b.lock.Release()
		}
	})
	return err
}

// Run registers this host with Pi and serves SSE commands, heartbeats, and
// active-thread monitoring until ctx is cancelled.
func (b *CLIBridge) Run(ctx context.Context) error {
	defer func() { _ = b.Close() }()

	if err := b.registerWithRetry(ctx); err != nil {
		return err
	}
	b.logger.Info("openalbert CLI bridge started",
		"environment_id", b.envID,
		"label", b.cfg.HostLabel,
		"t3_bin", b.t3Bin,
		"server_url", b.cfg.ServerURL,
	)

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		b.runSSELoop(ctx)
	}()
	go func() {
		defer wg.Done()
		b.runWatcherLoop(ctx)
	}()
	go func() {
		defer wg.Done()
		b.runHeartbeatLoop(ctx)
	}()
	wg.Wait()
	return nil
}

func (b *CLIBridge) registerWithRetry(ctx context.Context) error {
	backoff := b.ReconnectBaseDelay
	if backoff <= 0 {
		backoff = defaultReconnectBaseDelay
	}
	maxBackoff := b.ReconnectMaxDelay
	if maxBackoff < backoff {
		maxBackoff = defaultReconnectMaxDelay
	}
	for {
		regCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		resp, err := b.registerHost(regCtx)
		cancel()
		if err == nil {
			b.seedActiveThreads(resp.ActiveThreadIDs)
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		b.logger.Warn("CLI bridge host registration failed; retrying", "error", err, "backoff", backoff)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		backoff = nextBackoff(backoff, maxBackoff)
	}
}

func (b *CLIBridge) registerHost(ctx context.Context) (serverclient.T3HostRegisterResponse, error) {
	label := strings.TrimSpace(b.cfg.HostLabel)
	if label == "" {
		if hn, err := os.Hostname(); err == nil {
			label = strings.TrimSpace(hn)
		}
	}
	if label == "" {
		label = "unknown-host"
	}
	// Send empty Projects/Providers slices; the Pi backend preserves any previously
	// synced project/provider catalog on conflict while updating host liveness/label.
	return b.client.RegisterT3Host(ctx, serverclient.T3HostRegisterRequest{
		EnvironmentID: b.envID,
		Label:         label,
		Projects:      []serverclient.T3Project{},
		Providers:     []serverclient.T3Provider{},
	})
}

func (b *CLIBridge) activeThreadIDs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.activeThreads) == 0 {
		return []string{}
	}
	ids := make([]string, 0, len(b.activeThreads))
	for id := range b.activeThreads {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (b *CLIBridge) runSSELoop(ctx context.Context) {
	backoff := b.ReconnectBaseDelay
	if backoff <= 0 {
		backoff = defaultReconnectBaseDelay
	}
	maxBackoff := b.ReconnectMaxDelay
	if maxBackoff < backoff {
		maxBackoff = defaultReconnectMaxDelay
	}
	for {
		if ctx.Err() != nil {
			return
		}
		b.flushPendingResults(ctx)
		startedAt := time.Now()
		gotCommand := false
		err := b.client.StreamT3Commands(ctx, b.envID, func(cmd serverclient.T3Command) error {
			gotCommand = true
			cmdCtx, cancel := context.WithTimeout(ctx, commandTimeout)
			defer cancel()
			b.executeCommand(cmdCtx, cmd)
			return nil
		})
		if ctx.Err() != nil {
			return
		}
		if gotCommand || time.Since(startedAt) >= 5*time.Second {
			backoff = b.ReconnectBaseDelay
		}
		if err != nil {
			b.logger.Warn("CLI bridge SSE disconnected; reconnecting", "error", err, "backoff", backoff)
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		backoff = nextBackoff(backoff, maxBackoff)
	}
}

func (b *CLIBridge) runHeartbeatLoop(ctx context.Context) {
	interval := b.HeartbeatInterval
	if interval <= 0 {
		interval = defaultHeartbeatInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			hbCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			b.flushPendingResults(hbCtx)
			resp, err := b.client.HeartbeatT3Host(hbCtx, serverclient.T3HeartbeatRequest{
				EnvironmentID:   b.envID,
				ActiveThreadIDs: b.activeThreadIDs(),
			})
			cancel()
			if err != nil {
				b.logger.Warn("CLI bridge heartbeat failed", "error", err)
			} else {
				b.seedActiveThreads(resp.ActiveThreadIDs)
			}
		}
	}
}

func (b *CLIBridge) pollActiveThreads(ctx context.Context) {
	ids := b.activeThreadIDs()
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		pollCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		_, err := b.pollSingleThread(pollCtx, id, false)
		cancel()
		if err != nil {
			b.logger.Warn("CLI bridge poll failed", "thread_id", id, "error", err)
		}
	}
}

func (b *CLIBridge) runWatcherLoop(ctx context.Context) {
	// Immediately reconcile any active threads recovered from server registration.
	b.pollActiveThreads(ctx)

	interval := b.WatchInterval
	if interval <= 0 {
		interval = defaultCLIWatchInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.pollActiveThreads(ctx)
		}
	}
}

func (b *CLIBridge) beginCommand(commandID string) bool {
	if commandID == "" {
		return true
	}
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, ts := range b.completedCmds {
		if now.Sub(ts) > commandDedupTTL {
			if _, stillPending := b.pendingResults[id]; !stillPending {
				delete(b.completedCmds, id)
			}
		}
	}
	if b.inFlightCmds[commandID] {
		return false
	}
	if _, hasPending := b.pendingResults[commandID]; hasPending {
		return false
	}
	if _, done := b.completedCmds[commandID]; done {
		return false
	}
	b.inFlightCmds[commandID] = true
	return true
}

func (b *CLIBridge) finishAndQueueResult(commandID string, req serverclient.T3CommandResultRequest) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.inFlightCmds, commandID)
	if commandID != "" {
		b.completedCmds[commandID] = time.Now()
		b.pendingResults[commandID] = req
	}
}

func (b *CLIBridge) flushPendingResults(ctx context.Context) {
	b.mu.Lock()
	if len(b.pendingResults) == 0 {
		b.mu.Unlock()
		return
	}
	snapshot := make(map[string]serverclient.T3CommandResultRequest, len(b.pendingResults))
	for k, v := range b.pendingResults {
		snapshot[k] = v
	}
	b.mu.Unlock()
	for cmdID, req := range snapshot {
		if ctx.Err() != nil {
			return
		}
		if err := b.client.PostT3CommandResult(ctx, cmdID, req); err != nil {
			continue
		}
		b.mu.Lock()
		delete(b.pendingResults, cmdID)
		b.mu.Unlock()
	}
}

func (b *CLIBridge) executeCommand(ctx context.Context, cmd serverclient.T3Command) {
	if !b.beginCommand(cmd.CommandID) {
		return
	}
	result, err := b.dispatchCommand(ctx, cmd)
	req := serverclient.T3CommandResultRequest{
		EnvironmentID: b.envID,
		OK:            err == nil,
	}
	if err != nil {
		req.Error = err.Error()
	} else if len(result) > 0 && json.Valid(result) {
		req.Result = result
	} else {
		req.Result = json.RawMessage(`{}`)
	}
	b.finishAndQueueResult(cmd.CommandID, req)
	if cmd.CommandID == "" {
		return
	}
	if postErr := b.client.PostT3CommandResult(ctx, cmd.CommandID, req); postErr != nil {
		b.logger.Warn("CLI bridge failed to post command result; retained for retry", "command_id", cmd.CommandID, "error", postErr)
		return
	}
	b.mu.Lock()
	delete(b.pendingResults, cmd.CommandID)
	b.mu.Unlock()
}

func (b *CLIBridge) dispatchCommand(ctx context.Context, cmd serverclient.T3Command) (json.RawMessage, error) {
	switch cmd.Action {
	case "sync_capabilities":
		reg, err := b.registerHost(ctx)
		if err != nil {
			return nil, err
		}
		return json.Marshal(reg)
	case "thread_launch":
		return b.handleThreadLaunch(ctx, cmd.Payload)
	case "thread_send":
		return b.handleThreadSend(ctx, cmd.Payload)
	case "thread_read":
		return b.handleThreadRead(ctx, cmd.Payload)
	case "thread_control":
		return b.handleThreadControl(ctx, cmd.Payload)
	case "thread_respond":
		return nil, errors.New("t3 CLI bridge does not support thread_respond yet; answer pending questions directly in the T3 Code thread")
	default:
		return nil, fmt.Errorf("unsupported bridge command action %q", cmd.Action)
	}
}

func (b *CLIBridge) baseDirFlags() []string {
	if strings.TrimSpace(b.cfg.T3BaseDir) != "" {
		return []string{"--base-dir", b.cfg.T3BaseDir}
	}
	return nil
}

func writeTempSchemaFile(schema map[string]any) (string, func(), error) {
	if len(schema) == 0 {
		return "", func() {}, nil
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return "", func() {}, fmt.Errorf("marshal output_schema: %w", err)
	}
	f, err := os.CreateTemp("", "openalbert-schema-*.json")
	if err != nil {
		return "", func() {}, fmt.Errorf("create temp output_schema file: %w", err)
	}
	name := f.Name()
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", func() {}, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", func() {}, err
	}
	return name, func() { _ = os.Remove(name) }, nil
}

func (b *CLIBridge) handleThreadLaunch(ctx context.Context, payload json.RawMessage) (json.RawMessage, error) {
	var req threadLaunchPayload
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, fmt.Errorf("invalid thread_launch payload: %w", err)
		}
	}
	if len(req.AttachmentIDs) > 0 {
		return nil, errors.New("thread_launch attachment_ids are not supported by the CLI T3 bridge")
	}
	project := strings.TrimSpace(req.ProjectID)
	if project == "" {
		return nil, errors.New("thread_launch requires project_id")
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = "OpenAlbert Task"
	}
	message := req.Message
	if strings.TrimSpace(message) == "" {
		return nil, errors.New("thread_launch requires message")
	}

	args := []string{"thread", "start", "--json"}
	args = append(args, b.baseDirFlags()...)
	args = append(args, "--title", title)

	if ms := normalizeModelSelection(req.ModelSelection); len(ms) > 0 {
		if prov := firstNonEmpty(asString(ms["instanceId"]), asString(ms["provider"])); prov != "" {
			args = append(args, "--provider", prov)
		}
		if model := asString(ms["model"]); model != "" {
			args = append(args, "--model", model)
		}
		if opts, ok := ms["options"].(map[string]any); ok {
			keys := make([]string, 0, len(opts))
			for k := range opts {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				args = append(args, "--option", fmt.Sprintf("%s=%v", k, opts[k]))
			}
		}
	}

	if ws := normalizeWorkspaceStrategy(req.WorkspaceStrategy); len(ws) > 0 {
		if strings.EqualFold(asString(ws["type"]), "worktree") {
			baseRef := firstNonEmpty(asString(ws["baseRef"]), "HEAD")
			args = append(args, "--worktree", baseRef)
			if branch := asString(ws["branch"]); branch != "" {
				args = append(args, "--branch", branch)
			}
		}
	}

	schemaPath, cleanupSchema, err := writeTempSchemaFile(req.OutputSchema)
	if err != nil {
		return nil, err
	}
	defer cleanupSchema()
	if schemaPath != "" {
		args = append(args, "--output-schema", schemaPath)
	}

	// Pass message via stdin ("-") so arbitrary multiline UTF-8 works identically on Windows and Unix.
	args = append(args, project, "-")
	raw, err := b.runCLI(ctx, b.t3Bin, args, message)
	if err != nil {
		return nil, err
	}
	parsed := parseCLILaunchOutput(raw, req)
	if parsed.ThreadID == "" {
		return nil, errors.New("t3 thread start returned empty threadId")
	}
	if parsed.Link == "" && b.envID != "" {
		parsed.Link = fmt.Sprintf("https://app.t3.codes/threads/%s/%s", url.PathEscape(b.envID), url.PathEscape(parsed.ThreadID))
	}
	status := firstNonEmpty(parsed.Status, "running")

	launchHash := hashEventState(b.envID, parsed.ThreadID, "thread_launched", status, nil, "", "", "", parsed.Branch, "", nil, 0)
	b.trackThread(&trackedThread{
		threadID:        parsed.ThreadID,
		projectID:       firstNonEmpty(parsed.ProjectID, project),
		title:           firstNonEmpty(parsed.Title, title),
		link:            parsed.Link,
		providerID:      parsed.ProviderInstanceID,
		model:           parsed.Model,
		branch:          parsed.Branch,
		wasActive:       true,
		lastFingerprint: launchHash,
	})

	ev := serverclient.T3ThreadEvent{
		EventID:            "evt-" + launchHash,
		EnvironmentID:      b.envID,
		ThreadID:           parsed.ThreadID,
		Kind:               "thread_launched",
		Status:             status,
		Title:              firstNonEmpty(parsed.Title, title),
		Link:               parsed.Link,
		ProjectID:          firstNonEmpty(parsed.ProjectID, project),
		ProviderInstanceID: parsed.ProviderInstanceID,
		Model:              parsed.Model,
		Branch:             parsed.Branch,
		OccurredAt:         time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := b.client.PostT3Event(ctx, ev); err != nil {
		b.logger.Warn("CLI bridge failed to emit thread_launched event", "thread_id", parsed.ThreadID, "error", err)
	}

	return json.Marshal(map[string]any{
		"thread_id":            parsed.ThreadID,
		"link":                 parsed.Link,
		"title":                firstNonEmpty(parsed.Title, title),
		"status":               status,
		"branch":               parsed.Branch,
		"model":                parsed.Model,
		"provider_instance_id": parsed.ProviderInstanceID,
	})
}

func parseCLILaunchOutput(raw []byte, req threadLaunchPayload) launchResult {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return launchResult{}
	}
	if trimmed[0] == '{' {
		return parseLaunchResponse(trimmed, req)
	}
	// Fallback when CLI prints bare thread ID text.
	id := strings.TrimSpace(strings.Split(string(trimmed), "\n")[0])
	res := launchResult{ThreadID: id, ProjectID: req.ProjectID, Title: req.Title, Status: "running"}
	if ms := normalizeModelSelection(req.ModelSelection); len(ms) > 0 {
		res.Model = asString(ms["model"])
		res.ProviderInstanceID = firstNonEmpty(asString(ms["instanceId"]), asString(ms["provider"]))
	}
	return res
}

func (b *CLIBridge) handleThreadSend(ctx context.Context, payload json.RawMessage) (json.RawMessage, error) {
	var req struct {
		ThreadID      string         `json:"thread_id"`
		Message       string         `json:"message"`
		Mode          string         `json:"mode"`
		OutputSchema  map[string]any `json:"output_schema"`
		AttachmentIDs []string       `json:"attachment_ids"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("invalid thread_send payload: %w", err)
	}
	if len(req.AttachmentIDs) > 0 {
		return nil, errors.New("thread_send attachment_ids are not supported by the CLI T3 bridge")
	}
	threadID := strings.TrimSpace(req.ThreadID)
	if threadID == "" || strings.TrimSpace(req.Message) == "" {
		return nil, errors.New("thread_send requires thread_id and message")
	}

	args := []string{"thread", "send", "--json"}
	args = append(args, b.baseDirFlags()...)
	schemaPath, cleanupSchema, err := writeTempSchemaFile(req.OutputSchema)
	if err != nil {
		return nil, err
	}
	defer cleanupSchema()
	if schemaPath != "" {
		args = append(args, "--output-schema", schemaPath)
	}
	args = append(args, threadID, "-")

	raw, err := b.runCLI(ctx, b.t3Bin, args, req.Message)
	if err != nil {
		return nil, err
	}
	b.ensureTrackedActive(threadID)
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && json.Valid(trimmed) {
		return trimmed, nil
	}
	return json.Marshal(map[string]any{"thread_id": threadID, "delivery": "sent"})
}

func (b *CLIBridge) handleThreadRead(ctx context.Context, payload json.RawMessage) (json.RawMessage, error) {
	var req struct {
		ThreadID string `json:"thread_id"`
		Limit    int    `json:"limit"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("invalid thread_read payload: %w", err)
	}
	threadID := strings.TrimSpace(req.ThreadID)
	if threadID == "" {
		return nil, errors.New("thread_read requires thread_id")
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}
	args := []string{"thread", "show", "--json", "--messages", strconv.Itoa(limit)}
	args = append(args, b.baseDirFlags()...)
	args = append(args, threadID)
	raw, err := b.runCLI(ctx, b.t3Bin, args, "")
	if err != nil {
		return nil, err
	}
	return bytes.TrimSpace(raw), nil
}

func (b *CLIBridge) handleThreadControl(ctx context.Context, payload json.RawMessage) (json.RawMessage, error) {
	var req struct {
		ThreadID string `json:"thread_id"`
		Action   string `json:"action"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("invalid thread_control payload: %w", err)
	}
	threadID := strings.TrimSpace(req.ThreadID)
	action := strings.TrimSpace(req.Action)
	if threadID == "" || action == "" {
		return nil, errors.New("thread_control requires thread_id and action")
	}
	switch action {
	case "sync":
		ev, err := b.pollSingleThread(ctx, threadID, true)
		if err != nil {
			return nil, err
		}
		return json.Marshal(ev)
	default:
		return nil, fmt.Errorf("t3 CLI bridge does not support thread_control action %q", action)
	}
}

func (b *CLIBridge) trackThread(t *trackedThread) {
	b.mu.Lock()
	b.nextTrackGen++
	t.gen = b.nextTrackGen
	b.activeThreads[t.threadID] = t
	b.mu.Unlock()
}

func (b *CLIBridge) seedActiveThreads(threadIDs []string) {
	if len(threadIDs) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, rawID := range threadIDs {
		id := strings.TrimSpace(rawID)
		if id == "" {
			continue
		}
		if _, exists := b.activeThreads[id]; exists {
			continue
		}
		b.nextTrackGen++
		b.activeThreads[id] = &trackedThread{
			threadID:  id,
			gen:       b.nextTrackGen,
			wasActive: true,
		}
	}
}

func (b *CLIBridge) ensureTrackedActive(threadID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.activeThreads[threadID]
	if !ok {
		b.nextTrackGen++
		b.activeThreads[threadID] = &trackedThread{
			threadID:  threadID,
			gen:       b.nextTrackGen,
			wasActive: true,
		}
		return
	}
	t.wasActive = true
	t.lastFingerprint = ""
}

func (b *CLIBridge) lockThreadPoll(threadID string) func() {
	b.mu.Lock()
	pm, ok := b.threadPollMu[threadID]
	if !ok {
		pm = &sync.Mutex{}
		b.threadPollMu[threadID] = pm
	}
	b.mu.Unlock()
	pm.Lock()
	return pm.Unlock
}

func (b *CLIBridge) pollSingleThread(ctx context.Context, threadID string, forceEmit bool) (serverclient.T3ThreadEvent, error) {
	unlock := b.lockThreadPoll(threadID)
	defer unlock()

	args := []string{"thread", "show", "--json", "--messages", "10"}
	args = append(args, b.baseDirFlags()...)
	args = append(args, threadID)
	raw, err := b.runCLI(ctx, b.t3Bin, args, "")
	if err != nil {
		return serverclient.T3ThreadEvent{}, err
	}
	snap := parseThreadReadSnapshot(raw, threadID)

	b.mu.Lock()
	tracked := b.activeThreads[threadID]
	var wasActive bool
	var prevFingerprint string
	var meta trackedThread
	if tracked != nil {
		wasActive = tracked.wasActive
		prevFingerprint = tracked.lastFingerprint
		meta = *tracked
	}
	b.mu.Unlock()

	hasPending := snap.PendingRequestCount > 0 || strings.EqualFold(strings.TrimSpace(snap.Status), "waiting")
	var pendingPayload json.RawMessage
	if hasPending {
		pendingPayload = normalizePendingRequest("pending-"+threadID, nil)
	}

	kind, terminal := classifyThreadEvent(snap.Status, snap.LatestRunStatus, hasPending, wasActive)

	title := firstNonEmpty(snap.Title, meta.title)
	link := firstNonEmpty(snap.Link, meta.link)
	projectID := firstNonEmpty(snap.ProjectID, meta.projectID)
	providerID := firstNonEmpty(snap.ProviderInstanceID, meta.providerID)
	model := firstNonEmpty(snap.Model, meta.model)
	branch := firstNonEmpty(snap.Branch, meta.branch)

	stateHash := hashEventState(b.envID, threadID, kind, snap.Status, pendingPayload, snap.LatestRunID, snap.PRURL, snap.Error, branch, snap.Summary, snap.Output, snap.ItemCount)
	evID := "evt-" + stateHash
	if forceEmit {
		evID = uuid.NewString()
	}
	ev := serverclient.T3ThreadEvent{
		EventID:            evID,
		EnvironmentID:      b.envID,
		ThreadID:           threadID,
		Kind:               kind,
		Status:             snap.Status,
		Title:              title,
		Link:               link,
		ProjectID:          projectID,
		ProviderInstanceID: providerID,
		Model:              model,
		Branch:             branch,
		PRURL:              snap.PRURL,
		PendingRequest:     pendingPayload,
		Output:             snap.Output,
		Summary:            snap.Summary,
		Error:              snap.Error,
		OccurredAt:         time.Now().UTC().Format(time.RFC3339Nano),
	}
	if !forceEmit && stateHash == prevFingerprint {
		return ev, nil
	}
	if err := b.client.PostT3Event(ctx, ev); err != nil {
		return ev, err
	}

	b.mu.Lock()
	if t := b.activeThreads[threadID]; t != nil && t.gen == meta.gen {
		if terminal {
			delete(b.activeThreads, threadID)
		} else {
			t.title = title
			t.link = link
			t.projectID = projectID
			t.providerID = providerID
			t.model = model
			t.branch = branch
			t.lastFingerprint = stateHash
			if snap.Status == "running" || snap.Status == "starting" || snap.Status == "queued" || snap.Status == "waiting" {
				t.wasActive = true
			}
		}
	}
	b.mu.Unlock()
	return ev, nil
}

func asString(v any) string {
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}
