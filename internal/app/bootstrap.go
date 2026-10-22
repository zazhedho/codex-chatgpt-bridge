package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zazhedho/codex-chatgpt-bridge/internal/audit"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/codex"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/config"
	mcphandler "github.com/zazhedho/codex-chatgpt-bridge/internal/handler/mcp"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/repository/sqlite"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/security"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/service"
	"github.com/zazhedho/codex-chatgpt-bridge/internal/worker"
)

func Run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	var cloudflareVerifier security.AccessAssertionVerifier
	if cfg.AuthMode == config.AuthModeCloudflareAccess {
		cloudflareVerifier, err = security.NewCloudflareAccessVerifier(cfg.CloudflareIssuer, cfg.CloudflareAudience, cfg.CloudflareAllowedEmails)
		if err != nil {
			return fmt.Errorf("configure Cloudflare Access authentication: %w", err)
		}
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	store, err := sqlite.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.RecoverTasks(ctx); err != nil {
		return fmt.Errorf("recover interrupted tasks: %w", err)
	}
	if err := store.ExpireTaskOutputs(ctx, cfg.OutputRetentionDays); err != nil {
		return fmt.Errorf("expire retained task outputs: %w", err)
	}
	retentionCtx, stopRetention := context.WithCancel(context.Background())
	retentionDone := make(chan struct{})
	go func() {
		defer close(retentionDone)
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-retentionCtx.Done():
				return
			case <-ticker.C:
				cleanupCtx, cancel := context.WithTimeout(retentionCtx, 30*time.Second)
				if err := store.ExpireTaskOutputs(cleanupCtx, cfg.OutputRetentionDays); err != nil {
					logger.Warn("task output retention cleanup failed", "error", err.Error())
				}
				cancel()
			}
		}
	}()
	defer func() { stopRetention(); <-retentionDone }()
	auditLogger := audit.New(store)
	home, _ := os.UserHomeDir()
	codexHome := os.Getenv("CODEX_HOME")
	workspaces := service.NewWorkspaceService(store, auditLogger, home, codexHome, store)
	codexClient := codex.NewClient(cfg.CodexExecutable, logger)
	codexAdapter := codex.NewAdapter(codexClient, cfg.MaxOutputBytes)
	approvals := service.NewApprovalService(store, workspaces)
	trustedRoots := service.NewTrustedWorkspaceRootService(store, workspaces, auditLogger, home, codexHome)
	sessions := service.NewSessionService(store, workspaces, codexClient, auditLogger, trustedRoots)
	workers := worker.New(store, store, workspaces, codexAdapter, auditLogger, logger, cfg.MaxConcurrentTasks, cfg.MaxQueuedTasks, cfg.TaskTimeoutSeconds, cfg.MaxOutputBytes, sessions)
	tasks := service.NewTaskService(store, store, workspaces, approvals, workers, codexAdapter, auditLogger, cfg.MaxPromptBytes, cfg.MaxQueuedTasks, sessions)
	startedAt := time.Now()
	handler := mcphandler.NewHTTPHandler(mcphandler.Dependencies{Config: cfg, CloudflareVerifier: cloudflareVerifier, Workspaces: workspaces, Tasks: tasks, Sessions: sessions, Approvals: approvals, Audit: auditLogger, Codex: codexAdapter, StartedAt: startedAt, Logger: logger})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	listener, err := net.Listen("tcp", net.JoinHostPort(cfg.Host, fmt.Sprint(cfg.Port)))
	if err != nil {
		return fmt.Errorf("listen on loopback: %w", err)
	}
	workers.Start()
	logger.Info("bridge listening", "address", listener.Addr().String(), "data_dir", cfg.DataDir)
	serveErr := make(chan error, 1)
	go func() {
		err := server.Serve(listener)
		if !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()
	stopCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case <-stopCtx.Done():
	case err := <-serveErr:
		_ = listener.Close()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = workers.Shutdown(shutdownCtx)
		_ = codexAdapter.Close()
		return fmt.Errorf("HTTP server failed: %w", err)
	}
	logger.Info("bridge shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Warn("HTTP shutdown incomplete", "error", err.Error())
	}
	if err := workers.Shutdown(shutdownCtx); err != nil {
		logger.Warn("worker shutdown incomplete", "error", err.Error())
	}
	if err := codexAdapter.Close(); err != nil {
		logger.Warn("Codex App Server shutdown incomplete", "error", err.Error())
	}
	logger.Info("bridge stopped")
	return nil
}
