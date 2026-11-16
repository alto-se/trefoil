package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"trefoil/api/internal/build"
	"trefoil/api/internal/jobs"
	"trefoil/api/internal/modules"
	"trefoil/api/internal/schema"
	"trefoil/api/internal/server"
	"trefoil/api/internal/sessions"
	"trefoil/api/internal/settings"
	"trefoil/api/internal/store"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	settingsPath := flag.String("settings", os.Getenv("TREFOIL_SETTINGS"), "path to the backend settings file (backend.yml)")
	flag.Parse()

	cfg, err := settings.Load(*settingsPath)
	if err != nil {
		logger.Error("load settings", "err", err)
		os.Exit(1)
	}
	addr, dataDir := cfg.Addr, cfg.Storage.Local.Path

	validator, err := schema.NewConf()
	if err != nil {
		logger.Error("load schema", "err", err)
		os.Exit(1)
	}

	// Jobs and sessions are kept under the data dir so they survive restarts.
	jobManager, err := jobs.Open(filepath.Join(dataDir, "jobs"))
	if err != nil {
		logger.Error("load jobs", "err", err)
		os.Exit(1)
	}
	sessionManager, err := sessions.Open(filepath.Join(dataDir, "sessions"))
	if err != nil {
		logger.Error("load sessions", "err", err)
		os.Exit(1)
	}
	// Runs interrupted by the last shutdown were marked failed above; their
	// containers are no longer needed.
	if err := build.RemoveStoppedRuns(context.Background()); err != nil {
		logger.Warn("remove stopped run containers", "err", err)
	}

	srv := &http.Server{
		Addr: addr,
		Handler: server.New(logger, store.NewLocal(dataDir), validator, build.CLI{Env: cfg.AgentEnv()},
			filepath.Join(dataDir, "builds"), modules.NewLibrary(cfg.ModulesDir),
			server.WithProviders(cfg.LLMProvider), server.WithManagers(jobManager, sessionManager)),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Info("listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown error", "err", err)
	}
}
