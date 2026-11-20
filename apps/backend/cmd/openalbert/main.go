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

	"github.com/mishankov/OpenAlbert/shared/backup"
	"openalbert/apps/backend/internal/agent"
	"openalbert/apps/backend/internal/blob"
	"openalbert/apps/backend/internal/browser"
	"openalbert/apps/backend/internal/computer"
	"openalbert/apps/backend/internal/config"
	"openalbert/apps/backend/internal/desktopapi"
	"openalbert/apps/backend/internal/domain"
	"openalbert/apps/backend/internal/inspector"
	"openalbert/apps/backend/internal/integrations"
	"openalbert/apps/backend/internal/integrations/siwc"
	"openalbert/apps/backend/internal/knowledge"
	"openalbert/apps/backend/internal/orchestration"
	"openalbert/apps/backend/internal/storage"
	"openalbert/apps/backend/internal/telemetry"
	"openalbert/apps/backend/internal/widgets"

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
	server := &http.Server{Addr: c.HTTPAddr, Handler: telemetry.HTTP(logger, mux), ReadHeaderTimeout: 5 * time.Second}
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- server.ListenAndServe() }()
	defer func() {
		closeCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = server.Shutdown(closeCtx)
	}()
	databaseEvent := telemetry.Start(ctx, logger, "database.initialize")
	repo, err := storage.Open(databaseEvent.Context(), c.DatabaseURL)
	databaseEvent.End(err)
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
	var plan *siwc.Client
	if c.OpenAIAuthMode == "siwc" || c.OpenAIAuthMode == "auto" || (c.OpenAIAuthMode == "profiles" && c.Models.HasSubscription()) {
		plan, err = siwc.New(c.OpenAITokenStore, c.OpenAISiWCModel)
		if err != nil {
			return err
		}
		defer plan.Close()
	}
	// A typed nil must not be passed as a subscription interface.
	var subscription integrations.SubscriptionModel
	if plan != nil {
		subscription = plan
	}
	model, err := integrations.NewModelRouterWithPlan(c.Models, subscription)
	if err != nil {
		return err
	}
	billing := &integrations.BillingModel{Mode: c.OpenAIAuthMode, API: model, Cooldown: c.OpenAILaneCooldown}
	if plan != nil {
		billing.Plan = plan
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
	telegram, err := integrations.NewTelegram(integrations.TelegramConfig{Widgets: c.TelegramWidgets, Logger: logger, Token: c.TelegramBotToken, BaseURL: c.TelegramBaseURL, AllowedUserIDs: c.TelegramAllowedUserIDs, AllowedGroupIDs: c.TelegramAllowedGroupIDs, AllowBotMessages: c.TelegramAllowBotMessages})
	if err != nil {
		return err
	}
	telegram.ChatGPT = &integrations.ChatGPTControls{Billing: billing, OwnerID: c.TelegramOwnerUserID}
	if err = telegram.ConfigureCommands(ctx); err != nil {
		return fmt.Errorf("Telegram command setup failed: %w", err)
	}
	var logs domain.LogReader
	if c.VictoriaLogsURL != "" {
		logs, err = integrations.NewVictoriaLogs(c.VictoriaLogsURL, nil)
		if err != nil {
			return err
		}
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
	activities := &agent.Activities{Codemode: true, OwnerUserID: c.TelegramOwnerUserID, AllowedGroupIDs: c.TelegramAllowedGroupIDs, Browser: browsers, Repo: artifacts, Knowledge: knowledgeStore, Model: billing, Search: search, Messenger: telegram, Logger: logger, MinTaskInterval: c.TaskMinInterval, InspectorPublicURL: c.InspectorPublicURL}
	activities.Logs = logs
	if c.ComputerSSHAddress != "" {
		activities.Computer, err = computer.New(computer.Config{Address: c.ComputerSSHAddress, User: c.ComputerSSHUser, Password: c.ComputerSSHPassword, KeyFile: c.ComputerSSHKeyFile})
		if err != nil {
			return err
		}
	}
	if c.BackupStateDir != "" {
		activities.Backups = &backup.Reader{Dir: c.BackupStateDir}
	}
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
	activities.Widgets = &widgets.Controller{Repo: artifacts, Transport: telegram, TaskChanged: activities.TaskChanged}
	if browsers != nil {
		activities.Widgets.PrepareCloseTask = browsers.(*browser.Manager).PrepareCloseTask
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
	(&desktopapi.API{Repo: artifacts, Memory: knowledgeStore, OwnerID: c.TelegramOwnerUserID, Token: c.DesktopAPIToken, Billing: billing}).Register(mux)
	inspector.Register(mux, repo, artifacts)
	inspector.RegisterMemory(mux, repo, knowledgeStore)
	runtime.Start(ctx)
	defer runtime.Wait()
	defer cancel()
	ready.Store(true)
	event := telemetry.Start(ctx, logger, "application.started")
	event.Set("model_routes", c.Models.Routes)
	event.Set("web_search_enabled", c.YandexAPIKey != "")
	event.Set("log_search_enabled", logs != nil)
	event.Set("browser_enabled", c.BrowserEnabled)
	event.Set("computer_enabled", activities.Computer != nil)
	event.Set("telegram_allow_bot_messages", c.TelegramAllowBotMessages)
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
