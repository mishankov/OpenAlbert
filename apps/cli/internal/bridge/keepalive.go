package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/mishankov/OpenAlbert/apps/cli/internal/config"
)

const defaultKeepaliveInterval = 5 * time.Second

// KeepaliveBridge is a lightweight, cross-platform (macOS/Windows/Linux)
// watchdog that uses the official `t3` CLI to keep a dedicated persistent
// `openalbert acp` MCP bridge thread alive. Unlike CLIBridge, it does NOT
// acquire bridge.lock so `openalbert acp` owns the singleton MCP bridge and
// can execute real mid-turn `t3_thread_send(mode: "steer")` in near real-time.
type KeepaliveBridge struct {
	cfg      config.Config
	threadID string
	t3Bin    string
	interval time.Duration
	logger   *slog.Logger
	runCLI   CLIRunner
}

// NewKeepalive creates a keepalive watchdog for the given persistent T3 bridge thread ID.
func NewKeepalive(cfg config.Config, threadID string, interval time.Duration, logger *slog.Logger) (*KeepaliveBridge, error) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return nil, errors.New("keepalive thread ID is required")
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	t3Bin, err := ResolveT3Bin(cfg.T3Bin)
	if err != nil {
		return nil, err
	}
	if interval <= 0 {
		interval = defaultKeepaliveInterval
	}
	return &KeepaliveBridge{
		cfg:      cfg,
		threadID: threadID,
		t3Bin:    t3Bin,
		interval: interval,
		logger:   logger,
		runCLI:   defaultCLIRunner,
	}, nil
}

func (k *KeepaliveBridge) log() *slog.Logger {
	if k.logger != nil {
		return k.logger
	}
	return slog.Default()
}

// Run checks the bridge thread immediately and then every interval until ctx is cancelled.
func (k *KeepaliveBridge) Run(ctx context.Context) error {
	k.log().Info("openalbert bridge keepalive started", "thread_id", k.threadID, "interval", k.interval.String(), "t3_bin", k.t3Bin)
	k.checkOnce(ctx)

	ticker := time.NewTicker(k.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			k.checkOnce(ctx)
		}
	}
}

func (k *KeepaliveBridge) baseDirFlags() []string {
	if strings.TrimSpace(k.cfg.T3BaseDir) != "" {
		return []string{"--base-dir", k.cfg.T3BaseDir}
	}
	return nil
}

// checkOnce inspects the bridge thread via `t3 thread show` and triggers
// DefaultBridgeLivePrompt via `t3 thread send` whenever the thread is not active.
func (k *KeepaliveBridge) checkOnce(ctx context.Context) {
	checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	showArgs := []string{"thread", "show", "--json", "--messages", "0"}
	showArgs = append(showArgs, k.baseDirFlags()...)
	showArgs = append(showArgs, k.threadID)

	raw, err := k.runCLI(checkCtx, k.t3Bin, showArgs, "")
	if err != nil {
		if ctx.Err() == nil {
			k.log().Warn("bridge keepalive failed to inspect thread; will retry", "thread_id", k.threadID, "error", err)
		}
		return
	}

	status := parseKeepaliveThreadStatus(raw)
	switch status {
	case "running", "starting", "preparing", "queued":
		return
	}

	sendCtx, sendCancel := context.WithTimeout(ctx, 20*time.Second)
	defer sendCancel()

	sendArgs := []string{"thread", "send", "--json"}
	sendArgs = append(sendArgs, k.baseDirFlags()...)
	sendArgs = append(sendArgs, k.threadID, "-")
	if _, err := k.runCLI(sendCtx, k.t3Bin, sendArgs, config.DefaultBridgeLivePrompt); err != nil {
		if ctx.Err() == nil {
			k.log().Warn("bridge keepalive failed to wake live turn", "thread_id", k.threadID, "prev_status", status, "error", err)
		}
		return
	}
	k.log().Info("bridge keepalive woke persistent live turn", "thread_id", k.threadID, "prev_status", status)
}

func parseKeepaliveThreadStatus(raw []byte) string {
	var resp struct {
		Status string `json:"status"`
		Thread *struct {
			Status string `json:"status"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return ""
	}
	st := resp.Status
	if resp.Thread != nil && strings.TrimSpace(resp.Thread.Status) != "" {
		st = resp.Thread.Status
	}
	return strings.ToLower(strings.TrimSpace(st))
}

// String returns a human-readable description for logging/diagnostics.
func (k *KeepaliveBridge) String() string {
	return fmt.Sprintf("KeepaliveBridge(thread=%s, interval=%s)", k.threadID, k.interval)
}
