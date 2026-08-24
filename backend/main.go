// Package main is the URL shortener API.
//
// It is intentionally small and explicit: this project exists to learn
// how a real Kubernetes workload is shaped — probes, configuration,
// graceful shutdown — not to hide behind a framework.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func main() {
	// JSON logs to stdout. Kubernetes collects container stdout as pod
	// logs (kubectl logs), so structured JSON makes them easy to query.
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	port := envOr("PORT", "8080")

	// signal.NotifyContext cancels the context when the process receives
	// SIGTERM or SIGINT. Kubernetes sends SIGTERM when a pod is stopped,
	// so this is how "kubectl delete pod" turns into a clean shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := NewPostgresStore(ctx, PostgresConfigFromEnv(), logger)
	if err != nil {
		logger.Error("cannot start without database", "error", err)
		os.Exit(1)
	}
	defer store.Close()

	google := GoogleConfigFromEnv()
	if !google.Enabled() {
		logger.Warn("Google SSO disabled — set GOOGLE_CLIENT_ID and GOOGLE_CLIENT_SECRET to enable sign-in")
	}
	if google.Enabled() && string(google.SessionSecret) == "insecure-dev-secret" {
		logger.Warn("SESSION_SECRET is the default — sessions can be forged. Set a long random value in production!")
	}

	maxLinks := envOrInt("MAX_LINKS_PER_USER", 15)
	if maxLinks < 1 {
		maxLinks = 15
	}

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           NewHandlers(store, logger, google, maxLinks).Routes(),
		ReadHeaderTimeout: 5 * time.Second, // basic protection against slow-loris attacks
	}

	// Run the server in the background; main goroutine waits for a signal.
	serverErr := make(chan error, 1)
	go func() {
		logger.Info("server starting", "port", port)
		serverErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serverErr:
		logger.Error("server failed", "error", err)
		os.Exit(1)
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining connections")
	}

	// Graceful shutdown: stop accepting new requests, finish in-flight
	// ones, but never hang forever (10s deadline).
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("graceful shutdown failed", "error", err)
		os.Exit(1)
	}
	logger.Info("bye")
}

// envOr reads an environment variable or returns a default.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envOrInt reads an integer environment variable or returns a default.
func envOrInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return def
	}
	return n
}
