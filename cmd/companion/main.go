package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nicolasmarchal/wholphin-companion/internal/app"
	"github.com/nicolasmarchal/wholphin-companion/internal/config"
	"github.com/nicolasmarchal/wholphin-companion/internal/httpapi"
	"github.com/nicolasmarchal/wholphin-companion/internal/secure"
	"github.com/nicolasmarchal/wholphin-companion/internal/store"
	"github.com/nicolasmarchal/wholphin-companion/internal/upstream"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		runHealthcheck()
		return
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := config.Load()
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(2)
	}
	sealer, err := secure.NewSealer(cfg.DataKey)
	if err != nil {
		logger.Error("initialize encryption", "error", err)
		os.Exit(2)
	}
	database, err := store.Open(cfg.DatabasePath, sealer)
	if err != nil {
		logger.Error("open database", "error", err)
		os.Exit(2)
	}
	defer database.Close()

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 32
	transport.MaxIdleConnsPerHost = 8
	transport.IdleConnTimeout = 90 * time.Second
	client := &http.Client{Transport: transport}

	qb, err := upstream.NewQBittorrent(cfg.QBittorrentURL, cfg.QBittorrentUser, cfg.QBittorrentPass, client)
	if err != nil {
		logger.Error("initialize qBittorrent client", "error", err)
		os.Exit(2)
	}
	application := app.New(
		database,
		upstream.NewRadarr(cfg.Radarr, client),
		upstream.NewSonarr(cfg.Sonarr, client),
		upstream.NewJellyfin(cfg.JellyfinURL, cfg.JellyfinAPIKey, client),
		qb,
		app.Options{
			SessionTTL: cfg.SessionTTL, SelectionTTL: cfg.SelectionTTL,
			SearchTimeout: cfg.SearchTimeout, UpstreamTimeout: cfg.UpstreamTimeout,
			ReconcileEvery: cfg.ReconcileEvery, AllowedUsers: cfg.AllowedUsers,
			AllowAllUsers: cfg.AllowAllUsers, AllowCancel: cfg.AllowCancel,
		},
		logger,
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	application.Start(ctx)

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           httpapi.New(application, logger, cfg.WebhookSecret),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	errorsCh := make(chan error, 1)
	go func() {
		logger.Info("companion listening", "address", cfg.ListenAddr)
		errorsCh <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
	case err = <-errorsCh:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server stopped", "error", err)
		}
	}
	shutdownTimeout := cfg.UpstreamTimeout + 5*time.Second
	if shutdownTimeout < 10*time.Second {
		shutdownTimeout = 10 * time.Second
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown", "error", err)
	}
}

func runHealthcheck() {
	endpoint := os.Getenv("BFF_HEALTHCHECK_URL")
	if endpoint == "" {
		endpoint = "http://127.0.0.1:8090/healthz"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get(endpoint)
	if err != nil {
		os.Exit(1)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		os.Exit(1)
	}
}
