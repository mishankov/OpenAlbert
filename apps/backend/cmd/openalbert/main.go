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
	"openalbert/apps/backend/internal/t3"
	"openalbert/apps/backend/internal/telemetry"
	"openalbert/apps/backend/internal/widgets"
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
	var activeLogger atomic.Pointer[slog.Logger]
	activeLogger.Store(logger)
	server := &http.Server{
		Addr: c.HTTPAddr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			telemetry.HTTP(activeLogger.Load(), mux).ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- server.ListenAndServe() }()
	defer func() {
		closeCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = server.Shutdown(closeCtx)
	}()
	databaseEvent := telemetry.Start(ctx, logger, "database.initialize")
	repo, err := storage.Open(databaseEvent.Context(), c.DatabasePath)
	databaseEvent.End(err)
	if err != nil {
		return fmt.Errorf("database initialization failed: %w", err)
	}
	defer repo.Close()
	logger = telemetry.NewWithSink(os.Stdout, repo.WriteLog)
	slog.SetDefault(logger)
	activeLogger.Store(logger)
	artifacts, err := blob.New(repo, c.ArtifactsDir)
	if err != nil {
		return fmt.Errorf("artifact storage initialization failed: %w", err)
	}
	knowledgeStore, err := knowledge.NewWithSoulFile(c.KnowledgeDir, c.SoulFile)
	if err != nil {
		return fmt.Errorf("knowledge initialization failed: %w", err)
	}
	var plan *siwc.Client
	if c.OpenAIAuthMode == "siwc" || c.OpenAIAuthMode == "auto" || (c.OpenAIAuthMode == "profiles" && c.Models.HasSubscription()) {
		plan, err = siwc.NewWithCatalogProfile(c.OpenAITokenStore, c.OpenAISiWCModel, c.Models.SubscriptionCatalogProfile())
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
	var voice domain.SpeechSynthesizer
	if c.TTSBaseURL != "" {
		voice, err = integrations.NewTTS(integrations.TTSConfig{BaseURL: c.TTSBaseURL, APIKey: c.TTSAPIKey, Model: c.TTSModel, Voice: c.TTSVoice})
		if err != nil {
			return err
		}
	}
	search, err := integrations.NewSearch(integrations.SearchConfig{
		Provider: c.WebSearchProvider,
		Exa:      integrations.ExaConfig{APIKey: c.ExaAPIKey, Endpoint: c.ExaEndpoint, MonthlyBudgetUSD: c.ExaMonthlyBudgetUSD},
		Yandex:   integrations.YandexConfig{APIKey: c.YandexAPIKey, FolderID: c.YandexFolderID, Endpoint: c.YandexEndpoint},
	})
	if err != nil {
		return err
	}
	telegram, err := integrations.NewTelegram(integrations.TelegramConfig{Widgets: c.TelegramWidgets, Logger: logger, Token: c.TelegramBotToken, BaseURL: c.TelegramBaseURL, OwnerUserID: c.TelegramOwnerUserID, AllowedUserIDs: c.TelegramAllowedUserIDs, AllowedGroupIDs: c.TelegramAllowedGroupIDs, WorkspaceGroupID: c.TelegramWorkspaceGroupID, AllowBotMessages: c.TelegramAllowBotMessages})
	if err != nil {
		return err
	}
	telegram.ChatGPT = &integrations.ChatGPTControls{Billing: billing, OwnerID: c.TelegramOwnerUserID}
	if err = telegram.ConfigureCommands(ctx); err != nil {
		return fmt.Errorf("Telegram command setup failed: %w", err)
	}
	var browsers domain.Browser
	if c.BrowserEnabled {
		browsers, err = browser.New(ctx, browser.Config{Executable: c.BrowserExecutable, NoSandbox: c.BrowserNoSandbox})
		if err != nil {
			return err
		}
		defer browsers.Close()
	}
	activities := &agent.Activities{Codemode: true, OwnerUserID: c.TelegramOwnerUserID, AllowedGroupIDs: c.TelegramAllowedGroupIDs, WorkspaceGroupID: c.TelegramWorkspaceGroupID, Workspace: telegram, Browser: browsers, Repo: artifacts, Knowledge: knowledgeStore, Model: billing, Search: search, Messenger: telegram, Logger: logger, MinTaskInterval: c.TaskMinInterval, InspectorPublicURL: c.InspectorPublicURL}
	activities.Logs = repo
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
	runtime := &orchestration.Runtime{
		Repo:                        artifacts,
		Activities:                  activities,
		Telegram:                    telegram,
		TTS:                         voice,
		IdleTimeout:                 c.SessionIdleTimeout,
		CodingSessionStaleTimeout:   c.CodingSessionStaleTimeout,
		MaxSteps:                    c.MaxAgentSteps,
		InitiativeInterval:          c.InitiativeInterval,
		MemoryConsolidationInterval: c.MemoryConsolidationInterval,
		Logger:                      logger,
		OnPruneLogs: func(ctx context.Context) error {
			return repo.PruneLogs(ctx, time.Now().UTC().AddDate(0, 0, -30))
		},
	}
	activities.TaskChanged = runtime.NotifyTaskChanged
	t3ControlPlane := t3.New(repo, artifacts, t3.Config{OwnerUserID: c.TelegramOwnerUserID, Logger: logger, TaskChanged: activities.TaskChanged})
	activities.T3 = t3ControlPlane
	activities.Widgets = &widgets.Controller{Repo: artifacts, Transport: telegram, TaskChanged: activities.TaskChanged}
	if browsers != nil {
		activities.Widgets.PrepareCloseTask = browsers.(*browser.Manager).PrepareCloseTask
	}
	callbacks := integrations.NewCallbackRouter(c.TelegramOwnerUserID, c.TelegramWorkspaceGroupID, telegram.AnswerCallbackQuery)
	if c.TelegramWorkspaceGroupID < 0 && c.TelegramOwnerUserID > 0 {
		t3WorkspaceBridge := agent.NewT3WorkspaceBridge(agent.T3WorkspaceBridgeConfig{
			ControlPlane:     t3ControlPlane,
			Repo:             artifacts,
			Transport:        telegram,
			OwnerUserID:      c.TelegramOwnerUserID,
			WorkspaceGroupID: c.TelegramWorkspaceGroupID,
			Logger:           logger,
		})
		t3ControlPlane.AddObserver(t3WorkspaceBridge)
		callbacks.RegisterPrefix(agent.T3CallbackPrefix, t3WorkspaceBridge)
	}
	runtime.Callbacks = callbacks
	(&desktopapi.API{Repo: artifacts, Memory: knowledgeStore, OwnerID: c.TelegramOwnerUserID, Token: c.DesktopAPIToken, Billing: billing, Activities: activities, T3: t3ControlPlane, Logger: logger}).Register(mux)
	inspector.Register(mux, repo, artifacts)
	inspector.RegisterMemory(mux, repo, knowledgeStore)
	inspector.RegisterSearch(mux, repo, search, c.ExaMonthlyBudgetUSD)
	runtime.Start(ctx)
	defer runtime.Wait()
	defer cancel()
	ready.Store(true)
	searchProvider := ""
	if p, ok := search.(interface{ SearchProvider() string }); ok {
		searchProvider = p.SearchProvider()
	}
	event := telemetry.Start(ctx, logger, "application.started")
	event.Set("model_routes", c.Models.Routes)
	event.Set("web_search_enabled", searchProvider != "")
	if searchProvider != "" {
		event.Set("web_search_provider", searchProvider)
	}
	event.Set("log_search_enabled", true)
	event.Set("browser_enabled", c.BrowserEnabled)
	event.Set("computer_enabled", activities.Computer != nil)
	event.Set("telegram_allow_bot_messages", c.TelegramAllowBotMessages)
	event.Set("browser_limit", 5)
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
