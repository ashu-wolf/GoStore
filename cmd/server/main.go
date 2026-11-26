// Command server runs GoStore.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runServer(ctx, cfg, log)
}

func runServer(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	// Recovery finishes before the server starts listening.
	engine, report, err := storage.OpenWithOptions(cfg.DataDir, storage.Options{
		MaxPartSize:            cfg.MaxObjectSize,
		MaxMultipartObjectSize: cfg.MaxMultipartSize,
		MinPartSize:            cfg.MinPartSize,
		MaxUploadParts:         cfg.MaxUploadParts,
		UploadExpiry:           cfg.UploadExpiry,
		MinFreeSpace:           cfg.MinFreeSpace,
		DisableSpaceCheck:      cfg.MinFreeSpace == 0,
	})
	if err != nil {
		return fmt.Errorf("open storage: %w", err)
	}
	// Close storage only after all handlers and background workers have stopped.
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

	h := server.New(log, engine, server.Options{
		MaxObjectSize:         cfg.MaxObjectSize,
		MaxListKeys:           cfg.ListPageSize,
		MaxConcurrentRequests: cfg.MaxConcurrentRequests,
		IdleTransferTimeout:   cfg.IdleTransferTimeout,
		AccessKey:             cfg.AccessKey,
		SecretKey:             cfg.SecretKey,
		Region:                cfg.Region,
	})

	var pprofSrv *http.Server
	if cfg.PprofAddr != "" {
		pprofSrv = startPprof(cfg.PprofAddr, log)
		defer func() {
			pCtx, pCancel := context.WithTimeout(context.Background(), time.Second)
			defer pCancel()
			_ = pprofSrv.Shutdown(pCtx)
		}()
	}

	baseCtx, cancelActive := context.WithCancel(context.Background())
	defer cancelActive()

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           h,
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return err
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", ln.Addr().String())
		errCh <- srv.Serve(ln)
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
	}

	log.Info("shutting down")
	h.SetReady(false)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("shutdown deadline reached; canceling active requests", "error", err)
		cancelActive()
		_ = srv.Close()
	}
	if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	h.Wait()
	log.Info("stopped")
	return nil
}

func startPprof(addr string, log *slog.Logger) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Info("pprof listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("pprof failed", "error", err)
		}
	}()
	return srv
}
