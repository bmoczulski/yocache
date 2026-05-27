// Command yocache is a smart, writable cache server for Yocto/bitbake build
// farms (DL mirror + sstate cache, with mDNS discovery and federation planned).
//
// This is the bare-minimum skeleton: it serves a health endpoint and shuts
// down cleanly on SIGINT/SIGTERM. Real functionality is layered on from here.
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
)

func main() {
	addr := flag.String("addr", ":6768", "address the HTTP server listens on")
	dbPath := flag.String("db", "var/hashequiv/hashequiv.db", "path to the SQLite operational database")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	// Operational state lives in a single SQLite file. Today it backs only the
	// hash-equivalence store; inventory/peer/conflict tables join it later. Make
	// the parent dir so the default "var/" path works out of the box.
	if dir := filepath.Dir(*dbPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Error("cannot create database directory", "dir", dir, "err", err)
			os.Exit(1)
		}
	}
	store, err := openHashEquivStore(*dbPath)
	if err != nil {
		log.Error("cannot open database", "path", *dbPath, "err", err)
		os.Exit(1)
	}
	defer store.Close()
	log.Info("hashequiv store ready", "path", *dbPath)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
	})

	// Hash-equivalence server, spoken over WebSocket on this same port. Point a
	// build at it with BB_HASHSERVE = "ws://host:6768/hashequiv". See
	// hashequiv.go for the protocol and the (thin, in-memory) store.
	mux.HandleFunc("/hashequiv", newHashEquiv(store, log).handle)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Run the listener in the background so main can wait on a shutdown signal.
	errCh := make(chan error, 1)
	go func() {
		log.Info("yocache listening", "addr", *addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-errCh:
		log.Error("server failed", "err", err)
		os.Exit(1)
	case <-ctx.Done():
		log.Info("shutdown signal received, draining connections")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed", "err", err)
		os.Exit(1)
	}
	log.Info("shutdown complete")
}
