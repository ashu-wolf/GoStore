// Command server runs GoStore.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ashu-wolf/gostore/internal/config"
	"github.com/ashu-wolf/gostore/internal/server"
	"github.com/ashu-wolf/gostore/internal/storage"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("server failed", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	// Recovery finishes before the server starts listening.
	engine, report, err := storage.Open(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("open storage: %w", err)
	}
	// Engine must be closed only after all HTTP handlers have returned.
	defer func() {
		if err := engine.Close(); err != nil {
			log.Error("close storage", "error", err)
		}
	}()
	log.Info("storage recovered",
		"temp_removed", report.TempRemoved,
		"orphans_removed", report.OrphansRemoved,
		"journal_bytes_discarded", report.DiscardedBytes,
		"journal_compacted", report.Compacted,
		"damaged_objects", len(report.Damaged),
	)

	srv := &http.Server{
		Addr: cfg.Addr,
		Handler: server.New(log, engine, server.Options{
			MaxObjectSize: cfg.MaxObjectSize,
			MaxListKeys:   cfg.ListPageSize,
		}),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Addr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		_ = srv.Close()
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	log.Info("stopped")
	return nil
}
