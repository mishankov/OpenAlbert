package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/mishankov/OpenAlbert/apps/cli/internal/config"
	"github.com/mishankov/OpenAlbert/apps/cli/internal/mcpclient"
	"github.com/mishankov/OpenAlbert/apps/cli/internal/serverclient"
)

const (
	defaultWatchInterval      = 2 * time.Second
	defaultHeartbeatInterval  = 60 * time.Second
	defaultReconnectBaseDelay = 1 * time.Second
	defaultReconnectMaxDelay  = 30 * time.Second
	commandDedupTTL           = 5 * time.Minute
	commandTimeout            = 60 * time.Second
	maxSummaryRunes           = 2000
)

type trackedThread struct {
	threadID                   string
	gen                        uint64
	projectID                  string
	title                      string
	link                       string
	providerID                 string
	model                      string
	branch                     string
	wasActive                  bool
	lastObservedRunID          string
	outputSchemaRunID          string
	outputSchemaPendingNextRun bool
	outputSchemaAfterRunID     string
	lastFingerprint            string
	deliveredPendingID         string
	deliveredPendingPayload    json.RawMessage
}

// Bridge runs the 100%-LAN T3 Code orchestration bridge inside openalbert acp.
// Only one process per machine (per lock path) holds the flock; other instances
// stay purely interactive.
type Bridge struct {
	cfg    config.Config
	client *serverclient.Client
	logger *slog.Logger
	lock   *FileLock

	// Configurable timings (defaults applied in New).
	WatchInterval      time.Duration
	HeartbeatInterval  time.Duration
	ReconnectBaseDelay time.Duration
	ReconnectMaxDelay  time.Duration

	mu             sync.Mutex
	mcp            *mcpclient.Manager
	envID          string
	registered     bool
	nextTrackGen   uint64
	activeThreads  map[string]*trackedThread
	lastKnownRunID map[string]string
	threadPollMu   map[string]*sync.Mutex
	inFlightCmds   map[string]bool
	completedCmds  map[string]time.Time
	deferredCmds   []serverclient.T3Command
	deferredSet    map[string]bool
	pendingResults map[string]serverclient.T3CommandResultRequest
	closed         bool

	bgMu      sync.Mutex
	bgStarted bool
	bgCancel  context.CancelFunc
	bgWG      sync.WaitGroup
	signalCh  chan struct{}
	closeOnce sync.Once
}

// HasT3Tools reports whether an MCP manager exposes the required T3 Code orchestration tools.
func HasT3Tools(mgr *mcpclient.Manager) bool {
	if mgr == nil {
		return false
	}
	return mgr.HasRemoteTool("t3_environment_read") && mgr.HasRemoteTool("t3_thread_launch")
}

// New attempts to acquire the singleton bridge lock for cfg and returns a ready Bridge.
// If another instance already holds the lock, it returns (nil, false, nil).
func New(cfg config.Config, client *serverclient.Client, logger *slog.Logger) (*Bridge, bool, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	lock, acquired, err := AcquireLock(cfg.BridgeLockPath())
	if err != nil {
		return nil, false, err
	}
	if !acquired {
		return nil, false, nil
	}
	return &Bridge{
		cfg:                cfg,
		client:             client,
		logger:             logger,
		lock:               lock,
		WatchInterval:      defaultWatchInterval,
		HeartbeatInterval:  defaultHeartbeatInterval,
		ReconnectBaseDelay: defaultReconnectBaseDelay,
		ReconnectMaxDelay:  defaultReconnectMaxDelay,
		activeThreads:      map[string]*trackedThread{},
		lastKnownRunID:     map[string]string{},
		threadPollMu:       map[string]*sync.Mutex{},
		inFlightCmds:       map[string]bool{},
		completedCmds:      map[string]time.Time{},
		deferredSet:        map[string]bool{},
		pendingResults:     map[string]serverclient.T3CommandResultRequest{},
		signalCh:           make(chan struct{}, 1),
	}, true, nil
}

// Start begins the background SSE command stream, active-thread watcher, and heartbeat loops.
// It is idempotent and stops cleanly when ctx is canceled or Close is called.
func (b *Bridge) Start(ctx context.Context) {
	if b == nil {
		return
	}
	b.bgMu.Lock()
	defer b.bgMu.Unlock()
	if b.bgStarted {
		return
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.mu.Unlock()

	bgCtx, cancel := context.WithCancel(ctx)
	b.bgCancel = cancel
	b.bgStarted = true

	b.bgWG.Add(3)
	go func() {
		defer b.bgWG.Done()
		b.runSSELoop(bgCtx)
	}()
	go func() {
		defer b.bgWG.Done()
		b.runWatcherLoop(bgCtx)
	}()
	go func() {
		defer b.bgWG.Done()
		b.runHeartbeatLoop(bgCtx)
	}()
}

// Close stops background loops and releases the singleton file lock.
func (b *Bridge) Close() error {
	if b == nil {
		return nil
	}
	var releaseErr error
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closed = true
		b.mcp = nil
		b.mu.Unlock()

		b.bgMu.Lock()
		cancel := b.bgCancel
		b.bgMu.Unlock()
		if cancel != nil {
			cancel()
		}
		b.bgWG.Wait()
		if b.lock != nil {
			releaseErr = b.lock.Release()
		}
	})
	return releaseErr
}

// AttachMCP binds a T3-capable MCP manager to the bridge and wakes background loops.
func (b *Bridge) AttachMCP(mgr *mcpclient.Manager) {
	if b == nil || !HasT3Tools(mgr) {
		return
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.mcp = mgr
	b.mu.Unlock()
	b.notifySignal()
}

// DetachMCP clears the attached MCP manager if it matches mgr.
func (b *Bridge) DetachMCP(mgr *mcpclient.Manager) {
	if b == nil || mgr == nil {
		return
	}
	b.mu.Lock()
	if b.mcp == mgr {
		b.mcp = nil
	}
	b.mu.Unlock()
}

// EnvironmentID returns the last discovered T3 environment ID.
func (b *Bridge) EnvironmentID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.envID
}

// ActiveThreadIDs returns a sorted snapshot of currently tracked non-terminal threads.
func (b *Bridge) ActiveThreadIDs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.activeThreadIDsLocked()
}

func (b *Bridge) activeThreadIDsLocked() []string {
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

func (b *Bridge) currentMCP() *mcpclient.Manager {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.mcp
}

func (b *Bridge) notifySignal() {
	select {
	case b.signalCh <- struct{}{}:
	default:
	}
}

// Tick executes a zero-LLM bridge synchronization cycle inside an active ACP prompt turn:
//  1. Attaches mgr (if T3-capable) and registers/refreshes host capabilities if needed.
//  2. Retries any unacknowledged command result POSTs.
//  3. Sends a heartbeat with active thread IDs.
//  4. Executes any SSE commands deferred while outside an active turn, then drains ?once=true.
//  5. Polls all tracked active threads once and emits deduplicated state-change events.
func (b *Bridge) Tick(ctx context.Context, mgr *mcpclient.Manager) error {
	if b == nil {
		return nil
	}
	if HasT3Tools(mgr) {
		b.AttachMCP(mgr)
	}
	activeMCP := mgr
	if !HasT3Tools(activeMCP) {
		activeMCP = b.currentMCP()
	}
	if !HasT3Tools(activeMCP) {
		return errors.New("t3-code MCP tools not attached")
	}

	b.mu.Lock()
	needRegister := !b.registered || b.envID == ""
	envID := b.envID
	b.mu.Unlock()

	if needRegister {
		reg, err := b.syncCapabilities(ctx, activeMCP)
		if err != nil {
			return fmt.Errorf("bridge sync capabilities: %w", err)
		}
		envID = reg.EnvironmentID
	}

	b.flushPendingResults(ctx)

	if hbResp, err := b.client.HeartbeatT3Host(ctx, serverclient.T3HeartbeatRequest{
		EnvironmentID:   envID,
		ActiveThreadIDs: b.ActiveThreadIDs(),
	}); err != nil {
		b.logger.Warn("bridge tick heartbeat failed", "environment_id", envID, "error", err)
	} else {
		b.seedActiveThreads(hbResp.ActiveThreadIDs)
	}

	deferred := b.takeDeferredCommands()
	for i, cmd := range deferred {
		if ctx.Err() != nil {
			b.requeueDeferredCommands(deferred[i:])
			return ctx.Err()
		}
		b.executeCommand(ctx, activeMCP, cmd, false)
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}
	cmds, err := b.client.DrainT3Commands(ctx, envID)
	if err != nil {
		b.logger.Warn("bridge tick command drain failed", "environment_id", envID, "error", err)
	} else {
		for i, cmd := range cmds {
			if ctx.Err() != nil {
				b.requeueDeferredCommands(cmds[i:])
				return ctx.Err()
			}
			b.executeCommand(ctx, activeMCP, cmd, false)
		}
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}
	b.pollAllActiveThreads(ctx, activeMCP, false)
	return nil
}

func (b *Bridge) runSSELoop(ctx context.Context) {
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
		b.mu.Lock()
		envID := b.envID
		registered := b.registered
		mcpMgr := b.mcp
		b.mu.Unlock()

		if (!registered || envID == "") && HasT3Tools(mcpMgr) {
			regCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			reg, err := b.syncCapabilities(regCtx, mcpMgr)
			cancel()
			if err != nil {
				if isDeferrableOutsideTurnError(err) {
					b.logger.Info("bridge background registration deferred until next tick", "error", err)
				} else {
					b.logger.Warn("bridge background registration failed", "error", err)
				}
				if !waitCtxOrSignal(ctx, backoff, b.signalCh) {
					return
				}
				backoff = nextBackoff(backoff, maxBackoff)
				continue
			}
			envID = reg.EnvironmentID
			backoff = b.ReconnectBaseDelay
		}

		if envID == "" {
			select {
			case <-ctx.Done():
				return
			case <-b.signalCh:
				continue
			}
		}

		b.flushPendingResults(ctx)

		startedAt := time.Now()
		gotCommand := false
		err := b.client.StreamT3Commands(ctx, envID, func(cmd serverclient.T3Command) error {
			gotCommand = true
			cmdCtx, cancel := context.WithTimeout(ctx, commandTimeout)
			defer cancel()
			b.executeCommand(cmdCtx, b.currentMCP(), cmd, true)
			return nil
		})
		if ctx.Err() != nil {
			return
		}
		if gotCommand || time.Since(startedAt) >= 5*time.Second {
			backoff = b.ReconnectBaseDelay
		}
		if err != nil {
			b.logger.Warn("bridge SSE stream disconnected; reconnecting", "environment_id", envID, "backoff", backoff, "error", err)
		}
		if !waitCtxOrSignal(ctx, backoff, b.signalCh) {
			return
		}
		backoff = nextBackoff(backoff, maxBackoff)
	}
}

func (b *Bridge) runWatcherLoop(ctx context.Context) {
	interval := b.WatchInterval
	if interval <= 0 {
		interval = defaultWatchInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if len(b.ActiveThreadIDs()) == 0 {
			continue
		}
		mcpMgr := b.currentMCP()
		if !HasT3Tools(mcpMgr) || b.EnvironmentID() == "" {
			continue
		}
		pollCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		b.pollAllActiveThreads(pollCtx, mcpMgr, true)
		cancel()
	}
}

func (b *Bridge) runHeartbeatLoop(ctx context.Context) {
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
			envID := b.EnvironmentID()
			if envID == "" {
				continue
			}
			hbCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			b.flushPendingResults(hbCtx)
			hbResp, err := b.client.HeartbeatT3Host(hbCtx, serverclient.T3HeartbeatRequest{
				EnvironmentID:   envID,
				ActiveThreadIDs: b.ActiveThreadIDs(),
			})
			cancel()
			if err != nil {
				b.logger.Warn("bridge background heartbeat failed", "environment_id", envID, "error", err)
			} else {
				b.seedActiveThreads(hbResp.ActiveThreadIDs)
			}
		}
	}
}

func waitCtxOrSignal(ctx context.Context, d time.Duration, sig <-chan struct{}) bool {
	if d <= 0 {
		d = time.Second
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-sig:
		return true
	case <-timer.C:
		return true
	}
}

func nextBackoff(cur, max time.Duration) time.Duration {
	if cur <= 0 {
		return defaultReconnectBaseDelay
	}
	next := cur * 2
	if next > max {
		return max
	}
	return next
}

func (b *Bridge) beginCommand(commandID string) bool {
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

func (b *Bridge) deferCommand(cmd serverclient.T3Command) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.inFlightCmds, cmd.CommandID)
	if cmd.CommandID != "" && b.deferredSet[cmd.CommandID] {
		return
	}
	if cmd.CommandID != "" {
		b.deferredSet[cmd.CommandID] = true
	}
	b.deferredCmds = append(b.deferredCmds, cmd)
}

func (b *Bridge) takeDeferredCommands() []serverclient.T3Command {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.deferredCmds) == 0 {
		return nil
	}
	out := b.deferredCmds
	b.deferredCmds = nil
	b.deferredSet = map[string]bool{}
	return out
}

func (b *Bridge) finishAndQueueResult(commandID string, req serverclient.T3CommandResultRequest) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.inFlightCmds, commandID)
	if commandID != "" {
		b.completedCmds[commandID] = time.Now()
		b.pendingResults[commandID] = req
	}
}

func (b *Bridge) flushPendingResults(ctx context.Context) {
	b.mu.Lock()
	if len(b.pendingResults) == 0 {
		b.mu.Unlock()
		return
	}
	snapshot := make(map[string]serverclient.T3CommandResultRequest, len(b.pendingResults))
	for k, v := range b.pendingResults {
		snapshot[k] = v
	}
	envID := b.envID
	b.mu.Unlock()

	for cmdID, req := range snapshot {
		if ctx.Err() != nil {
			return
		}
		if req.EnvironmentID == "" {
			req.EnvironmentID = envID
		}
		if err := b.client.PostT3CommandResult(ctx, cmdID, req); err != nil {
			b.logger.Warn("bridge failed to post command result; will retry", "command_id", cmdID, "error", err)
			continue
		}
		b.mu.Lock()
		delete(b.pendingResults, cmdID)
		b.mu.Unlock()
	}
}

func (b *Bridge) requeueDeferredCommands(cmds []serverclient.T3Command) {
	if len(cmds) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, cmd := range cmds {
		if cmd.CommandID != "" {
			if b.deferredSet[cmd.CommandID] || b.inFlightCmds[cmd.CommandID] {
				continue
			}
			if _, hasPending := b.pendingResults[cmd.CommandID]; hasPending {
				continue
			}
			if _, done := b.completedCmds[cmd.CommandID]; done {
				continue
			}
			b.deferredSet[cmd.CommandID] = true
		}
		b.deferredCmds = append(b.deferredCmds, cmd)
	}
}

func isDeferrableOutsideTurnError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, mcpclient.ErrMCPTransport) {
		return true
	}
	var toolErr *mcpclient.ToolError
	if errors.As(err, &toolErr) {
		return strings.Contains(strings.ToLower(toolErr.Message), "parent_not_active")
	}
	return false
}

func (b *Bridge) executeCommand(ctx context.Context, mgr *mcpclient.Manager, cmd serverclient.T3Command, deferOnMCPUnavailable bool) {
	if ctx.Err() != nil {
		// Never clear inFlightCmds here because this invocation did not acquire the claim.
		b.requeueDeferredCommands([]serverclient.T3Command{cmd})
		return
	}
	if !b.beginCommand(cmd.CommandID) {
		return
	}
	result, err := b.dispatchCommand(ctx, mgr, cmd)
	if err != nil && (ctx.Err() != nil || (deferOnMCPUnavailable && isDeferrableOutsideTurnError(err))) {
		b.deferCommand(cmd)
		b.logger.Info("bridge deferring command until next active turn", "command_id", cmd.CommandID, "action", cmd.Action, "error", err)
		return
	}

	envID := b.EnvironmentID()
	req := serverclient.T3CommandResultRequest{
		EnvironmentID: envID,
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
		b.logger.Warn("bridge failed to post command result; retained for retry", "command_id", cmd.CommandID, "action", cmd.Action, "error", postErr)
		return
	}
	b.mu.Lock()
	delete(b.pendingResults, cmd.CommandID)
	b.mu.Unlock()
}

func (b *Bridge) dispatchCommand(ctx context.Context, mgr *mcpclient.Manager, cmd serverclient.T3Command) (json.RawMessage, error) {
	if !HasT3Tools(mgr) {
		return nil, fmt.Errorf("%w: t3-code MCP tools not connected", mcpclient.ErrMCPTransport)
	}
	switch cmd.Action {
	case "sync_capabilities":
		reg, err := b.syncCapabilities(ctx, mgr)
		if err != nil {
			return nil, err
		}
		return json.Marshal(reg)
	case "thread_launch":
		return b.handleThreadLaunch(ctx, mgr, cmd.CommandID, cmd.Payload)
	case "thread_send":
		return b.handleThreadSend(ctx, mgr, cmd.CommandID, cmd.Payload)
	case "thread_read":
		return b.handleThreadRead(ctx, mgr, cmd.Payload)
	case "thread_respond":
		return b.handleThreadRespond(ctx, mgr, cmd.Payload)
	case "thread_control":
		return b.handleThreadControl(ctx, mgr, cmd.CommandID, cmd.Payload)
	case "artifact_fetch":
		return b.handleArtifactFetch(ctx, mgr, cmd.Payload)
	default:
		return nil, fmt.Errorf("unsupported bridge command action %q", cmd.Action)
	}
}

// SyncCapabilities refreshes host capabilities, projects, and providers with OpenAlbert.
func (b *Bridge) SyncCapabilities(ctx context.Context, mgr *mcpclient.Manager) error {
	if b == nil {
		return nil
	}
	activeMCP := mgr
	if !HasT3Tools(activeMCP) {
		activeMCP = b.currentMCP()
	}
	if !HasT3Tools(activeMCP) {
		return errors.New("t3-code MCP tools not attached")
	}
	_, err := b.syncCapabilities(ctx, activeMCP)
	return err
}

func (b *Bridge) syncCapabilities(ctx context.Context, mgr *mcpclient.Manager) (serverclient.T3HostRegisterResponse, error) {
	envRaw, err := mgr.CallRemote(ctx, "t3_environment_read", map[string]any{})
	if err != nil {
		return serverclient.T3HostRegisterResponse{}, fmt.Errorf("t3_environment_read: %w", err)
	}
	var envInfo struct {
		EnvironmentID string `json:"environmentId"`
		ID            string `json:"id"`
		Label         string `json:"label"`
	}
	if err := json.Unmarshal(envRaw, &envInfo); err != nil {
		return serverclient.T3HostRegisterResponse{}, fmt.Errorf("parse t3_environment_read: %w", err)
	}
	envID := strings.TrimSpace(envInfo.EnvironmentID)
	if envID == "" {
		envID = strings.TrimSpace(envInfo.ID)
	}
	if envID == "" {
		return serverclient.T3HostRegisterResponse{}, errors.New("t3_environment_read returned empty environmentId")
	}

	projects, err := b.listAllProjects(ctx, mgr)
	if err != nil {
		return serverclient.T3HostRegisterResponse{}, err
	}

	providers, err := b.listProviders(ctx, mgr)
	if err != nil {
		return serverclient.T3HostRegisterResponse{}, err
	}

	label := strings.TrimSpace(b.cfg.HostLabel)
	if label == "" {
		label = strings.TrimSpace(envInfo.Label)
	}
	if label == "" {
		if hn, err := os.Hostname(); err == nil {
			label = strings.TrimSpace(hn)
		}
	}

	regResp, err := b.client.RegisterT3Host(ctx, serverclient.T3HostRegisterRequest{
		EnvironmentID: envID,
		Label:         label,
		Projects:      projects,
		Providers:     providers,
	})
	if err != nil {
		return serverclient.T3HostRegisterResponse{}, err
	}
	if regResp.EnvironmentID == "" {
		regResp.EnvironmentID = envID
	}

	b.mu.Lock()
	b.envID = envID
	b.registered = true
	b.mu.Unlock()
	b.seedActiveThreads(regResp.ActiveThreadIDs)
	b.notifySignal()

	return regResp, nil
}

func (b *Bridge) listAllProjects(ctx context.Context, mgr *mcpclient.Manager) ([]serverclient.T3Project, error) {
	if !mgr.HasRemoteTool("t3_project_list") {
		return []serverclient.T3Project{}, nil
	}
	var (
		out         []serverclient.T3Project
		seenCursors = map[int]bool{}
		cursor      int
	)
	for page := 0; page < 50; page++ {
		args := map[string]any{
			"limit": 100,
		}
		if page > 0 {
			args["cursor"] = cursor
		}
		raw, err := mgr.CallRemote(ctx, "t3_project_list", args)
		if err != nil {
			return nil, fmt.Errorf("t3_project_list: %w", err)
		}
		var resp struct {
			Projects []struct {
				ID            string `json:"id"`
				Title         string `json:"title"`
				WorkspaceRoot string `json:"workspaceRoot"`
				WorkspacePath string `json:"workspace_root"`
				ArchivedAt    any    `json:"archivedAt"`
				DeletedAt     any    `json:"deletedAt"`
			} `json:"projects"`
			NextCursor *int `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			return nil, fmt.Errorf("parse t3_project_list: %w", err)
		}
		for _, p := range resp.Projects {
			if p.DeletedAt != nil || p.ArchivedAt != nil || strings.TrimSpace(p.ID) == "" {
				continue
			}
			root := p.WorkspaceRoot
			if root == "" {
				root = p.WorkspacePath
			}
			out = append(out, serverclient.T3Project{
				ID:            p.ID,
				Title:         p.Title,
				WorkspaceRoot: root,
			})
		}
		if resp.NextCursor == nil || *resp.NextCursor <= 0 || seenCursors[*resp.NextCursor] {
			break
		}
		seenCursors[*resp.NextCursor] = true
		cursor = *resp.NextCursor
	}
	if out == nil {
		out = []serverclient.T3Project{}
	}
	return out, nil
}

func (b *Bridge) listProviders(ctx context.Context, mgr *mcpclient.Manager) ([]serverclient.T3Provider, error) {
	if !mgr.HasRemoteTool("orchestrator_capabilities") {
		return []serverclient.T3Provider{}, nil
	}
	raw, err := mgr.CallRemote(ctx, "orchestrator_capabilities", map[string]any{})
	if err != nil {
		return nil, fmt.Errorf("orchestrator_capabilities: %w", err)
	}
	var resp struct {
		Providers []struct {
			ProviderInstanceID string   `json:"providerInstanceId"`
			SnakeInstanceID    string   `json:"provider_instance_id"`
			DriverKind         string   `json:"driverKind"`
			SnakeDriverKind    string   `json:"driver_kind"`
			DisplayName        string   `json:"displayName"`
			SnakeDisplayName   string   `json:"display_name"`
			Constraints        []string `json:"constraints"`
			Models             []struct {
				ID      string            `json:"id"`
				Label   string            `json:"label"`
				Options []json.RawMessage `json:"options"`
			} `json:"models"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("parse orchestrator_capabilities: %w", err)
	}
	out := make([]serverclient.T3Provider, 0, len(resp.Providers))
	for _, p := range resp.Providers {
		if providerDisabled(p.Constraints) {
			continue
		}
		pid := firstNonEmpty(p.ProviderInstanceID, p.SnakeInstanceID)
		if pid == "" {
			continue
		}
		models := make([]serverclient.T3Model, 0, len(p.Models))
		for _, m := range p.Models {
			if strings.TrimSpace(m.ID) == "" {
				continue
			}
			label := m.Label
			if label == "" {
				label = m.ID
			}
			models = append(models, serverclient.T3Model{
				ID:      m.ID,
				Label:   label,
				Options: m.Options,
			})
		}
		out = append(out, serverclient.T3Provider{
			ProviderInstanceID: pid,
			DriverKind:         firstNonEmpty(p.DriverKind, p.SnakeDriverKind),
			DisplayName:        firstNonEmpty(p.DisplayName, p.SnakeDisplayName, pid),
			Models:             models,
		})
	}
	return out, nil
}

func providerDisabled(constraints []string) bool {
	for _, c := range constraints {
		if strings.Contains(strings.ToLower(c), "disabled") {
			return true
		}
	}
	return false
}

type threadLaunchPayload struct {
	ProjectID                string         `json:"project_id"`
	Title                    string         `json:"title"`
	Message                  string         `json:"message"`
	ModelSelection           map[string]any `json:"model_selection"`
	WorkspaceStrategy        map[string]any `json:"workspace_strategy"`
	OutputSchema             map[string]any `json:"output_schema"`
	RuntimeMode              string         `json:"runtime_mode"`
	InteractionMode          string         `json:"interaction_mode"`
	ConversationID           string         `json:"conversation_id"`
	TaskID                   string         `json:"task_id"`
	AttachmentIDs            []string       `json:"attachment_ids"`
	AttachmentConversationID string         `json:"attachment_conversation_id"`
}

func normalizeModelSelection(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := map[string]any{}
	for k, v := range in {
		switch k {
		case "provider_instance_id", "instance_id":
			if _, exists := out["instanceId"]; !exists {
				out["instanceId"] = v
			}
		case "model_id":
			if _, exists := out["model"]; !exists {
				out["model"] = v
			}
		default:
			out[k] = v
		}
	}
	return out
}

func normalizeWorkspaceStrategy(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := map[string]any{}
	for k, v := range in {
		switch k {
		case "base_ref":
			out["baseRef"] = v
		case "start_from_origin":
			out["startFromOrigin"] = v
		case "worktree_path":
			out["worktreePath"] = v
		default:
			out[k] = v
		}
	}
	return out
}

func (b *Bridge) handleThreadLaunch(ctx context.Context, mgr *mcpclient.Manager, commandID string, payload json.RawMessage) (json.RawMessage, error) {
	var req threadLaunchPayload
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, fmt.Errorf("invalid thread_launch payload: %w", err)
		}
	}
	message := req.Message
	if len(req.AttachmentIDs) > 0 {
		attachConvID := firstNonEmpty(req.AttachmentConversationID, req.ConversationID)
		paths, err := materializeAttachments(ctx, b.client, b.cfg, "launch-"+uuid.NewString()[:8], attachConvID, req.AttachmentIDs)
		if err != nil {
			return nil, err
		}
		message = appendAttachmentPathsToMessage(message, paths)
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = "OpenAlbert Task"
	}
	args := map[string]any{
		"title": title,
	}
	if commandID != "" {
		args["clientRequestId"] = commandID
	}
	if req.ProjectID != "" {
		args["projectId"] = req.ProjectID
	}
	if message != "" {
		args["message"] = message
	}
	if ms := normalizeModelSelection(req.ModelSelection); len(ms) > 0 {
		args["modelSelection"] = ms
	}
	if ws := normalizeWorkspaceStrategy(req.WorkspaceStrategy); len(ws) > 0 {
		args["workspaceStrategy"] = ws
	}
	if len(req.OutputSchema) > 0 {
		args["outputSchema"] = req.OutputSchema
	}
	if req.RuntimeMode != "" {
		args["runtimeMode"] = req.RuntimeMode
	}
	if req.InteractionMode != "" {
		args["interactionMode"] = req.InteractionMode
	}

	raw, err := mgr.CallRemote(ctx, "t3_thread_launch", args)
	if err != nil {
		return nil, err
	}
	parsed := parseLaunchResponse(raw, req)
	if parsed.ThreadID == "" {
		return nil, errors.New("t3_thread_launch returned empty threadId")
	}

	status := parsed.Status
	if status == "" {
		status = "running"
	}

	envID := b.EnvironmentID()
	if parsed.Link == "" && envID != "" {
		parsed.Link = fmt.Sprintf("https://app.t3.codes/threads/%s/%s", url.PathEscape(envID), url.PathEscape(parsed.ThreadID))
	}
	launchHash := hashEventState(envID, parsed.ThreadID, "thread_launched", status, nil, "", "", "", parsed.Branch, "", nil, 0)
	tracked := &trackedThread{
		threadID:          parsed.ThreadID,
		projectID:         firstNonEmpty(parsed.ProjectID, req.ProjectID),
		title:             firstNonEmpty(parsed.Title, title),
		link:              parsed.Link,
		providerID:        parsed.ProviderInstanceID,
		model:             parsed.Model,
		branch:            parsed.Branch,
		wasActive:         true,
		lastObservedRunID: parsed.RunID,
		lastFingerprint:   launchHash,
	}
	if len(req.OutputSchema) > 0 {
		if parsed.RunID != "" {
			tracked.outputSchemaRunID = parsed.RunID
		} else {
			tracked.outputSchemaPendingNextRun = true
		}
	}
	b.trackThread(tracked)

	if envID != "" {
		ev := serverclient.T3ThreadEvent{
			EventID:            "evt-" + launchHash,
			EnvironmentID:      envID,
			ThreadID:           parsed.ThreadID,
			Kind:               "thread_launched",
			Status:             status,
			Title:              firstNonEmpty(parsed.Title, title),
			Link:               parsed.Link,
			ProjectID:          firstNonEmpty(parsed.ProjectID, req.ProjectID),
			ProviderInstanceID: parsed.ProviderInstanceID,
			Model:              parsed.Model,
			Branch:             parsed.Branch,
			OccurredAt:         time.Now().UTC().Format(time.RFC3339Nano),
		}
		if err := b.client.PostT3Event(ctx, ev); err != nil {
			b.logger.Warn("bridge failed to emit thread_launched event", "thread_id", parsed.ThreadID, "error", err)
		}
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

type launchResult struct {
	ThreadID           string
	RunID              string
	ProjectID          string
	Link               string
	Title              string
	Status             string
	Branch             string
	Model              string
	ProviderInstanceID string
}

func parseLaunchResponse(raw json.RawMessage, req threadLaunchPayload) launchResult {
	var flat struct {
		ThreadID           string `json:"threadId"`
		SnakeThreadID      string `json:"thread_id"`
		RunID              string `json:"runId"`
		LatestRunID        string `json:"latestRunId"`
		ID                 string `json:"id"`
		ProjectID          string `json:"projectId"`
		SnakeProjectID     string `json:"project_id"`
		Link               string `json:"link"`
		Title              string `json:"title"`
		Status             string `json:"status"`
		Branch             string `json:"branch"`
		Model              string `json:"model"`
		ProviderInstanceID string `json:"providerInstanceId"`
		SnakeProviderID    string `json:"provider_instance_id"`
		ModelSelection     *struct {
			Model      string `json:"model"`
			InstanceID string `json:"instanceId"`
			Provider   string `json:"provider"`
		} `json:"modelSelection"`
		Thread *struct {
			ThreadID           string `json:"threadId"`
			LatestRunID        string `json:"latestRunId"`
			ID                 string `json:"id"`
			ProjectID          string `json:"projectId"`
			Link               string `json:"link"`
			Title              string `json:"title"`
			Status             string `json:"status"`
			Branch             string `json:"branch"`
			Model              string `json:"model"`
			ProviderInstanceID string `json:"providerInstanceId"`
		} `json:"thread"`
	}
	_ = json.Unmarshal(raw, &flat)
	out := launchResult{
		ThreadID:           firstNonEmpty(flat.ThreadID, flat.SnakeThreadID, flat.ID),
		RunID:              firstNonEmpty(flat.RunID, flat.LatestRunID),
		ProjectID:          firstNonEmpty(flat.ProjectID, flat.SnakeProjectID),
		Link:               flat.Link,
		Title:              flat.Title,
		Status:             flat.Status,
		Branch:             flat.Branch,
		Model:              flat.Model,
		ProviderInstanceID: firstNonEmpty(flat.ProviderInstanceID, flat.SnakeProviderID),
	}
	if flat.ModelSelection != nil {
		out.Model = firstNonEmpty(out.Model, flat.ModelSelection.Model)
		out.ProviderInstanceID = firstNonEmpty(out.ProviderInstanceID, flat.ModelSelection.InstanceID, flat.ModelSelection.Provider)
	}
	if flat.Thread != nil {
		out.ThreadID = firstNonEmpty(out.ThreadID, flat.Thread.ThreadID, flat.Thread.ID)
		out.RunID = firstNonEmpty(out.RunID, flat.Thread.LatestRunID)
		out.ProjectID = firstNonEmpty(out.ProjectID, flat.Thread.ProjectID)
		out.Link = firstNonEmpty(out.Link, flat.Thread.Link)
		out.Title = firstNonEmpty(out.Title, flat.Thread.Title)
		out.Status = firstNonEmpty(out.Status, flat.Thread.Status)
		out.Branch = firstNonEmpty(out.Branch, flat.Thread.Branch)
		out.Model = firstNonEmpty(out.Model, flat.Thread.Model)
		out.ProviderInstanceID = firstNonEmpty(out.ProviderInstanceID, flat.Thread.ProviderInstanceID)
	}
	if req.ModelSelection != nil {
		if out.Model == "" {
			for _, k := range []string{"model", "model_id"} {
				if m, ok := req.ModelSelection[k].(string); ok && m != "" {
					out.Model = m
					break
				}
			}
		}
		if out.ProviderInstanceID == "" {
			for _, k := range []string{"instanceId", "provider_instance_id", "provider"} {
				if p, ok := req.ModelSelection[k].(string); ok && p != "" {
					out.ProviderInstanceID = p
					break
				}
			}
		}
	}
	return out
}

func isSteerOnIdleThreadError(err error) bool {
	if err == nil || isDeferrableOutsideTurnError(err) {
		return false
	}
	var toolErr *mcpclient.ToolError
	if !errors.As(err, &toolErr) {
		return false
	}
	msg := strings.ToLower(toolErr.Message)
	for _, needle := range []string{
		"idle",
		"not running",
		"no active",
		"no running",
		"not active",
		"not currently running",
		"no active turn",
		"no running turn",
		"nothing to steer",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

func (b *Bridge) handleThreadSend(ctx context.Context, mgr *mcpclient.Manager, commandID string, payload json.RawMessage) (json.RawMessage, error) {
	var req struct {
		ThreadID                 string         `json:"thread_id"`
		Message                  string         `json:"message"`
		Mode                     string         `json:"mode"`
		OutputSchema             map[string]any `json:"output_schema"`
		AttachmentIDs            []string       `json:"attachment_ids"`
		AttachmentConversationID string         `json:"attachment_conversation_id"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("invalid thread_send payload: %w", err)
	}
	req.ThreadID = strings.TrimSpace(req.ThreadID)
	if req.ThreadID == "" || strings.TrimSpace(req.Message) == "" {
		return nil, errors.New("thread_send requires thread_id and message")
	}
	message := req.Message
	if len(req.AttachmentIDs) > 0 {
		paths, err := materializeAttachments(ctx, b.client, b.cfg, req.ThreadID, req.AttachmentConversationID, req.AttachmentIDs)
		if err != nil {
			return nil, err
		}
		message = appendAttachmentPathsToMessage(message, paths)
	}
	args := map[string]any{
		"threadId": req.ThreadID,
		"message":  message,
	}
	if commandID != "" {
		args["clientRequestId"] = commandID
	}
	if req.Mode != "" {
		args["mode"] = req.Mode
	}
	if len(req.OutputSchema) > 0 {
		args["outputSchema"] = req.OutputSchema
	}
	effectiveSteer := req.Mode == "steer"
	raw, err := mgr.CallRemote(ctx, "t3_thread_send", args)
	if err != nil && req.Mode == "steer" && isSteerOnIdleThreadError(err) {
		b.logger.Info("bridge thread_send steer fell back to auto on idle thread", "thread_id", req.ThreadID, "error", err)
		fallbackArgs := make(map[string]any, len(args))
		for k, v := range args {
			fallbackArgs[k] = v
		}
		fallbackArgs["mode"] = "auto"
		if commandID != "" {
			fallbackArgs["clientRequestId"] = commandID + ":auto"
		}
		raw, err = mgr.CallRemote(ctx, "t3_thread_send", fallbackArgs)
		effectiveSteer = false
	}
	if err != nil {
		return nil, err
	}
	var sendResp struct {
		RunID       string `json:"runId"`
		LatestRunID string `json:"latestRunId"`
	}
	_ = json.Unmarshal(raw, &sendResp)
	runID := firstNonEmpty(sendResp.RunID, sendResp.LatestRunID)
	b.ensureTrackedActive(req.ThreadID, effectiveSteer, len(req.OutputSchema) > 0, runID)
	return raw, nil
}

func (b *Bridge) handleThreadRead(ctx context.Context, mgr *mcpclient.Manager, payload json.RawMessage) (json.RawMessage, error) {
	var req struct {
		ThreadID string `json:"thread_id"`
		View     string `json:"view"`
		Limit    int    `json:"limit"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("invalid thread_read payload: %w", err)
	}
	req.ThreadID = strings.TrimSpace(req.ThreadID)
	if req.ThreadID == "" {
		return nil, errors.New("thread_read requires thread_id")
	}
	args := map[string]any{
		"threadId": req.ThreadID,
	}
	if req.View != "" {
		args["view"] = req.View
	}
	if req.Limit > 0 {
		args["limit"] = req.Limit
	}
	return mgr.CallRemote(ctx, "t3_thread_read", args)
}

func (b *Bridge) handleThreadRespond(ctx context.Context, mgr *mcpclient.Manager, payload json.RawMessage) (json.RawMessage, error) {
	var req struct {
		ThreadID  string         `json:"thread_id"`
		RequestID string         `json:"request_id"`
		Answers   map[string]any `json:"answers"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("invalid thread_respond payload: %w", err)
	}
	req.ThreadID = strings.TrimSpace(req.ThreadID)
	req.RequestID = strings.TrimSpace(req.RequestID)
	if req.ThreadID == "" || req.RequestID == "" {
		return nil, errors.New("thread_respond requires thread_id and request_id")
	}
	args := map[string]any{
		"action":    "respond",
		"threadId":  req.ThreadID,
		"requestId": req.RequestID,
		"answers":   req.Answers,
	}
	raw, err := mgr.CallRemote(ctx, "t3_pending_request", args)
	if err != nil {
		return nil, err
	}
	b.ensureTrackedActive(req.ThreadID, true, false, "")
	return raw, nil
}

func (b *Bridge) handleThreadControl(ctx context.Context, mgr *mcpclient.Manager, commandID string, payload json.RawMessage) (json.RawMessage, error) {
	var req struct {
		ThreadID string `json:"thread_id"`
		Action   string `json:"action"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("invalid thread_control payload: %w", err)
	}
	req.ThreadID = strings.TrimSpace(req.ThreadID)
	req.Action = strings.TrimSpace(req.Action)
	if req.ThreadID == "" || req.Action == "" {
		return nil, errors.New("thread_control requires thread_id and action")
	}
	switch req.Action {
	case "interrupt":
		args := map[string]any{
			"threadId": req.ThreadID,
		}
		if commandID != "" {
			args["clientRequestId"] = commandID
		}
		return mgr.CallRemote(ctx, "t3_thread_interrupt", args)
	case "settle", "archive":
		raw, err := mgr.CallRemote(ctx, "t3_thread_organize", map[string]any{
			"threadId": req.ThreadID,
			"action":   req.Action,
		})
		if err != nil {
			return nil, err
		}
		b.untrackThread(req.ThreadID)
		return raw, nil
	case "sync":
		ev, err := b.pollSingleThread(ctx, mgr, req.ThreadID, true, false)
		if err != nil {
			return nil, err
		}
		return json.Marshal(ev)
	default:
		return nil, fmt.Errorf("unsupported thread_control action %q", req.Action)
	}
}

func (b *Bridge) handleArtifactFetch(ctx context.Context, mgr *mcpclient.Manager, payload json.RawMessage) (json.RawMessage, error) {
	lookupWorktree := func(ctx context.Context, threadID string) string {
		if strings.TrimSpace(threadID) == "" || !mgr.HasRemoteTool("t3_thread_read") {
			return ""
		}
		raw, err := mgr.CallRemote(ctx, "t3_thread_read", map[string]any{
			"threadId": threadID,
			"view":     "messages",
			"limit":    1,
		})
		if err != nil {
			return ""
		}
		var resp struct {
			WorktreePath string `json:"worktreePath"`
			Thread       *struct {
				WorktreePath string `json:"worktreePath"`
			} `json:"thread"`
		}
		if json.Unmarshal(raw, &resp) != nil {
			return ""
		}
		if resp.Thread != nil && strings.TrimSpace(resp.Thread.WorktreePath) != "" {
			return strings.TrimSpace(resp.Thread.WorktreePath)
		}
		return strings.TrimSpace(resp.WorktreePath)
	}
	return fetchLocalArtifact(ctx, b.client, b.cfg, payload, lookupWorktree)
}

func (b *Bridge) seedActiveThreads(threadIDs []string) {
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
			threadID:          id,
			gen:               b.nextTrackGen,
			wasActive:         true,
			lastObservedRunID: b.lastKnownRunID[id],
		}
	}
}

func (b *Bridge) trackThread(t *trackedThread) {
	b.mu.Lock()
	b.nextTrackGen++
	t.gen = b.nextTrackGen
	if t.lastObservedRunID != "" {
		b.lastKnownRunID[t.threadID] = t.lastObservedRunID
	} else if prev, ok := b.lastKnownRunID[t.threadID]; ok {
		t.lastObservedRunID = prev
	}
	b.activeThreads[t.threadID] = t
	b.mu.Unlock()
}

func (b *Bridge) ensureTrackedActive(threadID string, preserveOutputSchema bool, hasOutputSchema bool, runID string) {
	b.mu.Lock()
	b.nextTrackGen++
	t, ok := b.activeThreads[threadID]
	if !ok {
		t = &trackedThread{
			threadID:          threadID,
			lastObservedRunID: b.lastKnownRunID[threadID],
		}
		b.activeThreads[threadID] = t
	} else if t.lastObservedRunID == "" {
		t.lastObservedRunID = b.lastKnownRunID[threadID]
	}
	t.gen = b.nextTrackGen
	t.wasActive = true
	t.deliveredPendingID = ""
	t.deliveredPendingPayload = nil
	if hasOutputSchema {
		if runID != "" {
			t.outputSchemaRunID = runID
			t.outputSchemaPendingNextRun = false
			t.outputSchemaAfterRunID = ""
			t.lastObservedRunID = runID
			b.lastKnownRunID[threadID] = runID
		} else {
			t.outputSchemaRunID = ""
			t.outputSchemaPendingNextRun = true
			t.outputSchemaAfterRunID = t.lastObservedRunID
		}
	} else if !preserveOutputSchema {
		t.outputSchemaPendingNextRun = false
		t.outputSchemaAfterRunID = ""
		if runID == "" || runID != t.outputSchemaRunID {
			t.outputSchemaRunID = ""
		}
		if runID != "" {
			t.lastObservedRunID = runID
			b.lastKnownRunID[threadID] = runID
		}
	}
	b.mu.Unlock()
}

func (b *Bridge) lockThreadPoll(threadID string) func() {
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

func (b *Bridge) untrackThread(threadID string) {
	b.mu.Lock()
	delete(b.activeThreads, threadID)
	b.mu.Unlock()
}

func (b *Bridge) pollAllActiveThreads(ctx context.Context, mgr *mcpclient.Manager, background bool) {
	ids := b.ActiveThreadIDs()
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		if _, err := b.pollSingleThread(ctx, mgr, id, false, background); err != nil {
			if background && errors.Is(err, mcpclient.ErrMCPTransport) {
				b.logger.Debug("bridge background poll deferred", "thread_id", id, "error", err)
				return
			}
			b.logger.Warn("bridge thread poll failed", "thread_id", id, "error", err)
		}
	}
}

func (b *Bridge) pollSingleThread(ctx context.Context, mgr *mcpclient.Manager, threadID string, forceEmit bool, background bool) (serverclient.T3ThreadEvent, error) {
	unlockPoll := b.lockThreadPoll(threadID)
	defer unlockPoll()

	readRaw, err := mgr.CallRemote(ctx, "t3_thread_read", map[string]any{
		"threadId": threadID,
		"view":     "messages",
		"limit":    10,
	})
	if err != nil {
		return serverclient.T3ThreadEvent{}, err
	}

	snap := parseThreadReadSnapshot(readRaw, threadID)

	b.mu.Lock()
	if snap.LatestRunID != "" {
		b.lastKnownRunID[threadID] = snap.LatestRunID
	}
	tracked := b.activeThreads[threadID]
	var wasActive bool
	var prevFingerprint string
	var meta trackedThread
	if tracked != nil {
		if snap.LatestRunID != "" {
			if tracked.outputSchemaPendingNextRun && snap.LatestRunID != tracked.outputSchemaAfterRunID {
				tracked.outputSchemaRunID = snap.LatestRunID
				tracked.outputSchemaPendingNextRun = false
				tracked.outputSchemaAfterRunID = ""
			}
			tracked.lastObservedRunID = snap.LatestRunID
		}
		wasActive = tracked.wasActive
		prevFingerprint = tracked.lastFingerprint
		meta = *tracked
	}
	envID := b.envID
	b.mu.Unlock()

	var pendingID string
	var pendingPayload json.RawMessage
	var pendingReadOK bool
	if mgr.HasRemoteTool("t3_pending_request") {
		listRaw, listErr := mgr.CallRemote(ctx, "t3_pending_request", map[string]any{
			"action":   "list",
			"threadId": threadID,
		})
		if listErr != nil {
			if background && isDeferrableOutsideTurnError(listErr) {
				return serverclient.T3ThreadEvent{}, listErr
			}
		} else {
			reqIDs := parsePendingRequestIDs(listRaw)
			if len(reqIDs) > 0 {
				pendingID = reqIDs[0]
				readReqRaw, readReqErr := mgr.CallRemote(ctx, "t3_pending_request", map[string]any{
					"action":    "read",
					"threadId":  threadID,
					"requestId": pendingID,
				})
				if readReqErr != nil {
					if background && isDeferrableOutsideTurnError(readReqErr) {
						return serverclient.T3ThreadEvent{}, readReqErr
					}
					// Preserve the previously delivered full payload ONLY if it belongs to the exact same pendingID.
					if meta.deliveredPendingID == pendingID && len(meta.deliveredPendingPayload) > 0 {
						pendingPayload = meta.deliveredPendingPayload
					} else {
						pendingPayload = normalizePendingRequest(pendingID, nil)
					}
				} else {
					pendingReadOK = true
					pendingPayload = normalizePendingRequest(pendingID, readReqRaw)
				}
			}
		}
	}

	// Antigravity/subagent runs pause with status="completed" while waiting on
	// pendingBackgroundTasks or child thread waits; keep them running until those settle.
	hasBackgroundWork := snap.PendingBackgroundTasksCount > 0 || snap.BlockedOnThreadWait
	effectiveStatus := snap.Status
	if hasBackgroundWork && pendingID == "" {
		switch strings.ToLower(strings.TrimSpace(effectiveStatus)) {
		case "", "completed", "idle":
			effectiveStatus = "running"
		}
	}

	kind, terminal := classifyThreadEvent(effectiveStatus, snap.LatestRunStatus, pendingID != "", wasActive)
	if hasBackgroundWork && kind == "thread_completed" {
		kind = "thread_progress"
		terminal = false
		effectiveStatus = "running"
	}

	// If a queued schema-bearing run has not started yet (we still observe the preceding run),
	// keep watching instead of terminating on the preceding run's terminal status (completed or failed).
	queuedSchemaAwaitingStart := meta.outputSchemaPendingNextRun && meta.outputSchemaAfterRunID != "" && snap.LatestRunID == meta.outputSchemaAfterRunID
	if queuedSchemaAwaitingStart && terminal {
		kind = "thread_progress"
		terminal = false
	}

	expectsOutput := (meta.outputSchemaPendingNextRun && !queuedSchemaAwaitingStart) ||
		(meta.outputSchemaRunID != "" && (snap.LatestRunID == "" || snap.LatestRunID == meta.outputSchemaRunID))

	// When a run launched with output_schema completes, retrieve its structured output via t3_thread_wait(untilOutput: true).
	// If output is still outstanding (timed out, empty, wait error, or wait tool unavailable), retain the thread for another poll.
	if kind == "thread_completed" && len(snap.Output) == 0 && expectsOutput {
		if !mgr.HasRemoteTool("t3_thread_wait") {
			kind = "thread_progress"
			terminal = false
		} else {
			waitArgs := map[string]any{
				"threadId":    threadID,
				"timeoutMs":   1000,
				"untilOutput": true,
			}
			if snap.LatestRunID != "" {
				waitArgs["runId"] = snap.LatestRunID
			}
			waitRaw, waitErr := mgr.CallRemote(ctx, "t3_thread_wait", waitArgs)
			if waitErr != nil {
				if background && isDeferrableOutsideTurnError(waitErr) {
					return serverclient.T3ThreadEvent{}, waitErr
				}
				kind = "thread_progress"
				terminal = false
			} else {
				var waitResp struct {
					Output   json.RawMessage `json:"output"`
					TimedOut bool            `json:"timedOut"`
				}
				if json.Unmarshal(waitRaw, &waitResp) == nil {
					snap.Output = validNonNullJSON(waitResp.Output)
				}
				if len(snap.Output) == 0 {
					kind = "thread_progress"
					terminal = false
				}
			}
		}
	}

	title := firstNonEmpty(snap.Title, meta.title)
	link := firstNonEmpty(snap.Link, meta.link)
	projectID := firstNonEmpty(snap.ProjectID, meta.projectID)
	providerID := firstNonEmpty(snap.ProviderInstanceID, meta.providerID)
	model := firstNonEmpty(snap.Model, meta.model)
	branch := firstNonEmpty(snap.Branch, meta.branch)

	stateHash := hashEventState(envID, threadID, kind, effectiveStatus, pendingPayload, snap.LatestRunID, snap.PRURL, snap.Error, branch, snap.Summary, snap.Output, snap.ItemCount)

	evID := "evt-" + stateHash
	if forceEmit {
		evID = uuid.NewString()
	}
	ev := serverclient.T3ThreadEvent{
		EventID:            evID,
		EnvironmentID:      envID,
		ThreadID:           threadID,
		Kind:               kind,
		Status:             effectiveStatus,
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

	if envID != "" {
		if err := b.client.PostT3Event(ctx, ev); err != nil {
			return ev, err
		}
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
			if pendingID == "" {
				t.deliveredPendingID = ""
				t.deliveredPendingPayload = nil
			} else if pendingReadOK {
				t.deliveredPendingID = pendingID
				t.deliveredPendingPayload = append(json.RawMessage(nil), pendingPayload...)
			} else if t.deliveredPendingID != pendingID {
				t.deliveredPendingID = ""
				t.deliveredPendingPayload = nil
			}
			if effectiveStatus == "running" || effectiveStatus == "starting" || effectiveStatus == "queued" || effectiveStatus == "waiting" {
				t.wasActive = true
			}
		}
	}
	b.mu.Unlock()

	return ev, nil
}

func normalizePendingRequest(pendingID string, raw json.RawMessage) json.RawMessage {
	out := map[string]any{
		"request_id": pendingID,
	}
	if len(raw) > 0 && json.Valid(raw) {
		var parsed map[string]any
		if json.Unmarshal(raw, &parsed) == nil && parsed != nil {
			for k, v := range parsed {
				if k == "requestId" {
					continue
				}
				out[k] = v
			}
			out["request_id"] = pendingID
		}
	}
	encoded, _ := json.Marshal(out)
	return encoded
}

type threadReadSnapshot struct {
	ThreadID                    string
	ProjectID                   string
	Title                       string
	Link                        string
	Status                      string
	LatestRunID                 string
	LatestRunStatus             string
	ProviderInstanceID          string
	Model                       string
	Branch                      string
	PRURL                       string
	Summary                     string
	Error                       string
	Output                      json.RawMessage
	ItemCount                   int
	PendingRequestCount         int
	PendingBackgroundTasksCount int
	BlockedOnThreadWait         bool
}

func parseThreadReadSnapshot(raw json.RawMessage, fallbackThreadID string) threadReadSnapshot {
	var resp struct {
		Thread *struct {
			ThreadID           string `json:"threadId"`
			ProjectID          string `json:"projectId"`
			Title              string `json:"title"`
			Link               string `json:"link"`
			Status             string `json:"status"`
			LatestRunID        string `json:"latestRunId"`
			ProviderInstanceID string `json:"providerInstanceId"`
			Model              string `json:"model"`
			Branch             string `json:"branch"`
			PRURL              string `json:"prUrl"`
			LinkedPullRequest  *struct {
				URL string `json:"url"`
			} `json:"linkedPullRequest"`
			PullRequests []struct {
				URL string `json:"url"`
			} `json:"pullRequests"`
			ItemCount              int               `json:"itemCount"`
			PendingRequestCount    int               `json:"pendingRequestCount"`
			PendingBackgroundTasks []json.RawMessage `json:"pendingBackgroundTasks"`
			BlockedOnThreadWait    bool              `json:"blockedOnThreadWait"`
			Error                  string            `json:"error"`
		} `json:"thread"`
		RecentRuns []struct {
			RunID  string          `json:"runId"`
			Status string          `json:"status"`
			Output json.RawMessage `json:"output"`
			Error  string          `json:"error"`
		} `json:"recentRuns"`
		Items []struct {
			Type string `json:"type"`
			Role string `json:"role"`
			Text string `json:"text"`
		} `json:"items"`
		Messages []struct {
			Role string `json:"role"`
			Text string `json:"text"`
		} `json:"messages"`
		// Flat fallbacks for simpler responses.
		Status  string          `json:"status"`
		Summary string          `json:"summary"`
		Error   string          `json:"error"`
		Output  json.RawMessage `json:"output"`
		PRURL   string          `json:"prUrl"`
	}
	_ = json.Unmarshal(raw, &resp)

	snap := threadReadSnapshot{
		ThreadID: fallbackThreadID,
		Status:   resp.Status,
		Summary:  resp.Summary,
		Error:    resp.Error,
		Output:   validNonNullJSON(resp.Output),
		PRURL:    resp.PRURL,
	}
	if resp.Thread != nil {
		snap.ThreadID = firstNonEmpty(resp.Thread.ThreadID, fallbackThreadID)
		snap.ProjectID = resp.Thread.ProjectID
		snap.Title = resp.Thread.Title
		snap.Link = resp.Thread.Link
		snap.Status = firstNonEmpty(resp.Thread.Status, snap.Status)
		snap.LatestRunID = resp.Thread.LatestRunID
		snap.ProviderInstanceID = resp.Thread.ProviderInstanceID
		snap.Model = resp.Thread.Model
		snap.Branch = resp.Thread.Branch
		snap.ItemCount = resp.Thread.ItemCount
		snap.PendingRequestCount = resp.Thread.PendingRequestCount
		snap.PendingBackgroundTasksCount = len(resp.Thread.PendingBackgroundTasks)
		snap.BlockedOnThreadWait = resp.Thread.BlockedOnThreadWait
		snap.Error = firstNonEmpty(resp.Thread.Error, snap.Error)
		if resp.Thread.LinkedPullRequest != nil && resp.Thread.LinkedPullRequest.URL != "" {
			snap.PRURL = resp.Thread.LinkedPullRequest.URL
		} else if len(resp.Thread.PullRequests) > 0 && resp.Thread.PullRequests[0].URL != "" {
			snap.PRURL = resp.Thread.PullRequests[0].URL
		} else if resp.Thread.PRURL != "" {
			snap.PRURL = resp.Thread.PRURL
		}
	}
	if len(resp.RecentRuns) > 0 {
		latest := resp.RecentRuns[0]
		snap.LatestRunStatus = latest.Status
		if snap.LatestRunID == "" {
			snap.LatestRunID = latest.RunID
		}
		if len(snap.Output) == 0 {
			snap.Output = validNonNullJSON(latest.Output)
		}
		snap.Error = firstNonEmpty(snap.Error, latest.Error)
	}
	if snap.Summary == "" {
		// t3_thread_read returns items newest-first; pick the first assistant message with text.
		for _, item := range resp.Items {
			t := strings.ToLower(item.Type)
			r := strings.ToLower(item.Role)
			if strings.TrimSpace(item.Text) == "" {
				continue
			}
			if t == "assistant_message" || t == "agent_message" || r == "assistant" {
				snap.Summary = truncateRunes(strings.TrimSpace(item.Text), maxSummaryRunes)
				break
			}
		}
	}
	if snap.Summary == "" && len(resp.Messages) > 0 {
		// t3 thread show --json returns messages oldest-first; scan from newest to oldest.
		for i := len(resp.Messages) - 1; i >= 0; i-- {
			m := resp.Messages[i]
			if strings.EqualFold(strings.TrimSpace(m.Role), "assistant") && strings.TrimSpace(m.Text) != "" {
				snap.Summary = truncateRunes(strings.TrimSpace(m.Text), maxSummaryRunes)
				break
			}
		}
	}
	return snap
}

func parsePendingRequestIDs(raw json.RawMessage) []string {
	var resp struct {
		RequestIDs []string `json:"requestIds"`
		SnakeIDs   []string `json:"request_ids"`
		Requests   []struct {
			RequestID string `json:"requestId"`
			ID        string `json:"id"`
		} `json:"requests"`
	}
	if json.Unmarshal(raw, &resp) != nil {
		return nil
	}
	if len(resp.RequestIDs) > 0 {
		return resp.RequestIDs
	}
	if len(resp.SnakeIDs) > 0 {
		return resp.SnakeIDs
	}
	var out []string
	for _, r := range resp.Requests {
		if id := firstNonEmpty(r.RequestID, r.ID); id != "" {
			out = append(out, id)
		}
	}
	return out
}

func classifyThreadEvent(status, latestRunStatus string, hasPendingRequest, wasActive bool) (kind string, terminal bool) {
	if hasPendingRequest {
		return "thread_waiting_input", false
	}
	st := strings.ToLower(strings.TrimSpace(status))
	runSt := strings.ToLower(strings.TrimSpace(latestRunStatus))
	switch st {
	case "completed":
		return "thread_completed", true
	case "failed", "cancelled", "interrupted", "rolled_back", "error":
		return "thread_failed", true
	case "idle":
		switch runSt {
		case "failed", "cancelled", "interrupted", "rolled_back", "error":
			return "thread_failed", true
		case "completed":
			return "thread_completed", true
		default:
			if wasActive {
				return "thread_completed", true
			}
			return "thread_progress", false
		}
	default:
		return "thread_progress", false
	}
}

func hashEventState(envID, threadID, kind, status string, pendingPayload json.RawMessage, runID, prURL, errStr, branch, summary string, output json.RawMessage, itemCount int) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%d",
		envID, threadID, kind, status, string(pendingPayload), runID, prURL, errStr, branch, summary, string(output), itemCount)))
	return hex.EncodeToString(h[:16])
}

func validNonNullJSON(raw json.RawMessage) json.RawMessage {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" || !json.Valid(raw) {
		return nil
	}
	return raw
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
