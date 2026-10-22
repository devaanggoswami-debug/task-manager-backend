package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"taskmanager/backend/internal/api"
	"taskmanager/backend/internal/gemini"
	"taskmanager/backend/internal/store"
)

type config struct {
	databaseURL string
	httpAddr    string
	corsOrigin  string
	geminiKey   string
	geminiModel string
}

func loadConfig() config {
	return config{
		databaseURL: envOr("DATABASE_URL", "postgres://localhost:5432/taskmanager?sslmode=disable"),
		httpAddr:    envOr("HTTP_ADDR", "127.0.0.1:8080"),
		corsOrigin:  envOr("CORS_ORIGIN", "http://127.0.0.1:3100"),
		// The key is optional: without it the chat assistant stays disabled
		// and the chat endpoint returns its static help reply. It is read
		// only from the environment and never logged.
		geminiKey:   os.Getenv("GEMINI_API_KEY"),
		geminiModel: envOr("GEMINI_MODEL", gemini.DefaultModel),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		logger.Error("server exited", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg := loadConfig()
	warnIfNotLoopback(logger, cfg.httpAddr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, cfg.databaseURL)
	if err != nil {
		return errors.New("invalid DATABASE_URL")
	}
	defer pool.Close()

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		return err
	}

	var assistant api.Assistant
	if cfg.geminiKey != "" {
		// Keep the assistant interface nil (not a typed nil) when disabled,
		// so the handler's nil check works.
		assistant = gemini.New(cfg.geminiKey, cfg.geminiModel)
		logger.Info("chat assistant enabled", "model", cfg.geminiModel)
	} else {
		logger.Info("chat assistant disabled: GEMINI_API_KEY not set")
	}

	srv := &http.Server{
		Addr:              cfg.httpAddr,
		Handler:           api.NewHandler(store.NewPostgresStore(pool), cfg.corsOrigin, logger, assistant),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		// WriteTimeout must exceed the assistant turn budget (assistantTimeout,
		// 55s) so the handler can still write its reply after the Gemini client
		// has used its full per-attempt timeout and retry budget.
		WriteTimeout: 65 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.httpAddr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	return srv.Shutdown(shutdownCtx)
}

// warnIfNotLoopback flags an HTTP_ADDR override that exposes this
// unauthenticated API beyond the local machine.
func warnIfNotLoopback(logger *slog.Logger, addr string) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return
	}
	if host == "localhost" {
		return
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return
	}
	logger.Warn("HTTP_ADDR is not a loopback address; this API has no authentication", "addr", addr)
}
