package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"openalbert/internal/agent"
	"openalbert/internal/blob"
	"openalbert/internal/browser"
	"openalbert/internal/config"
	"openalbert/internal/domain"
	"openalbert/internal/inspector"
	"openalbert/internal/integrations"
	"openalbert/internal/knowledge"
	"openalbert/internal/orchestration"
	"openalbert/internal/storage"
	"openalbert/internal/telemetry"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
)

func main() {
	if err := run(); err != nil {
		slog.Error("OpenAlbert stopped", "error", err.Error())
		os.Exit(1)
	}
}
func run() error {
	logger := telemetry.New(os.Stdout)
	slog.SetDefault(logger)
	c, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var ready atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	server := &http.Server{Addr: c.HTTPAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- server.ListenAndServe() }()
	defer func() {
		closeCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = server.Shutdown(closeCtx)
	}()
	repo, err := storage.Open(ctx, c.DatabaseURL)
	if err != nil {
		return errors.New("database initialization failed")
	}
	defer repo.Close()
	artifacts, err := blob.New(ctx, repo, c.S3Endpoint, c.S3AccessKey, c.S3SecretKey, c.S3Bucket, c.S3Secure)
	if err != nil {
		return errors.New("S3 initialization failed")
	}
	knowledgeStore, err := knowledge.NewWithSoulFile(c.KnowledgeDir, c.SoulFile)
	if err != nil {
		return errors.New("knowledge initialization failed")
	}
	model, err := integrations.NewOpenAI(integrations.OpenAIConfig{BaseURL: c.LLMBaseURL, APIKey: c.LLMAPIKey, Model: c.LLMModel})
	if err != nil {
		return err
	}
	var speech domain.SpeechRecognizer
	if c.STTBaseURL != "" {
		speech, err = integrations.NewSTT(integrations.STTConfig{BaseURL: c.STTBaseURL, APIKey: c.STTAPIKey, Model: c.STTModel, Revision: c.STTRevision})
		if err != nil {
			return err
		}
	}
	search, err := integrations.NewSearch(integrations.YandexConfig{APIKey: c.YandexAPIKey, FolderID: c.YandexFolderID, Endpoint: c.YandexEndpoint})
	if err != nil {
		return err
	}
	telegram, err := integrations.NewTelegram(integrations.TelegramConfig{Token: c.TelegramBotToken, BaseURL: c.TelegramBaseURL, AllowedUserIDs: c.TelegramAllowedUserIDs, AllowedGroupIDs: c.TelegramAllowedGroupIDs})
	if err != nil {
		return err
	}
	temporalClient, err := client.DialContext(ctx, client.Options{HostPort: c.TemporalAddress, Namespace: c.TemporalNamespace, Logger: sdkLogger{logger}})
	if err != nil {
		return errors.New("Temporal connection failed")
	}
	defer temporalClient.Close()
	var browsers domain.Browser
	if c.BrowserEnabled {
		browsers, err = browser.New(ctx, browser.Config{Executable: c.BrowserExecutable, NoSandbox: c.BrowserNoSandbox})
		if err != nil {
			return err
		}
		defer browsers.Close()
	}
	activities := &agent.Activities{OwnerUserID: c.TelegramOwnerUserID, AllowedGroupIDs: c.TelegramAllowedGroupIDs, Browser: browsers, Repo: artifacts, Knowledge: knowledgeStore, Model: model, Search: search, Messenger: telegram, Logger: logger, MinTaskInterval: c.TaskMinInterval, ModelName: c.LLMModel}
	activities.STT = speech
	activities.STTModel = c.STTModel
	activities.TaskChanged = func(ctx context.Context, scope, id string) error {
		err := temporalClient.SignalWorkflow(ctx, orchestration.TaskWorkflowID(id), "", orchestration.TaskSignal, "changed")
		var missing *serviceerror.NotFound
		if errors.As(err, &missing) {
			return nil
		}
		return err
	}
	runtime := &orchestration.Runtime{Repo: artifacts, Activities: activities, Telegram: telegram, Client: temporalClient, TaskQueue: c.TemporalTaskQueue, IdleTimeout: c.SessionIdleTimeout, MaxSteps: c.MaxAgentSteps, InitiativeInterval: c.InitiativeInterval, Logger: logger}
	worker := runtime.Worker()
	if err = worker.Start(); err != nil {
		return errors.New("Temporal worker startup failed")
	}
	defer worker.Stop()
	sttWorker := runtime.STTWorker()
	if err = sttWorker.Start(); err != nil {
		return errors.New("STT worker startup failed")
	}
	defer sttWorker.Stop()
	if browsers != nil {
		for _, w := range runtime.BrowserWorkers() {
			if err = w.Start(); err != nil {
				return errors.New("browser worker startup failed")
			}
			defer w.Stop()
		}
	}
	inspector.Register(mux, repo, artifacts)
	runtime.Start(ctx)
	defer runtime.Wait()
	defer cancel()
	ready.Store(true)
	event := telemetry.Start(ctx, logger, "application.started")
	event.Set("model", c.LLMModel)
	event.Set("web_search_enabled", c.YandexAPIKey != "")
	event.Set("browser_enabled", c.BrowserEnabled)
	event.Set("browser_limit", 5)
	event.Set("namespace", c.TemporalNamespace)
	event.Set("task_queue", c.TemporalTaskQueue)
	event.End(nil)
	select {
	case <-ctx.Done():
		ready.Store(false)
		return nil
	case err := <-serverErrors:
		cancel()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("health server failed: %w", err)
	}
}

// SDK messages are operational metadata. Never pass provider payloads into this logger.
type sdkLogger struct{ logger *slog.Logger }

func (l sdkLogger) Debug(msg string, keyvals ...interface{}) { l.logger.Debug(msg, keyvals...) }
func (l sdkLogger) Info(msg string, keyvals ...interface{})  { l.logger.Info(msg, keyvals...) }
func (l sdkLogger) Warn(msg string, keyvals ...interface{})  { l.logger.Warn(msg, keyvals...) }
func (l sdkLogger) Error(msg string, keyvals ...interface{}) { l.logger.Error(msg, keyvals...) }
