// Command yocache is a smart, writable cache server for Yocto/bitbake build
// farms (DL mirror + sstate cache, with mDNS discovery and federation planned).
//
// This is the bare-minimum skeleton: it serves a health endpoint and shuts
// down cleanly on SIGINT/SIGTERM. Real functionality is layered on from here.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// buildReport mirrors the JSON sent by yocache.bbclass. The class POSTs one
// report per event (BuildStarted/BuildCompleted/MetadataEvent), not an
// end-of-build aggregate, so every report carries the event name plus a
// stringified dump of the raw bitbake event.
//
// Type and Metadata are only populated for bb.event.MetadataEvent. Metadata
// is left raw because its shape depends on the MetadataEvent subtype:
// MissedSstate carries {"missed": [...], "found": [...]}, others carry
// whatever they carry.
type buildReport struct {
	Event     string  `json:"event"`
	TS        float64 `json:"ts"`
	BuildName string  `json:"build_name"`
	Machine   string  `json:"machine"`
	Distro    string  `json:"distro"`

	// Provenance — best-effort identity gathered out-of-band by the bbclass so
	// builds can be grouped by user/machine even under a shared-uid kas
	// container, where USER/uid/pid all collapse. Any field may be empty; none
	// is sufficient alone, combined they're compelling. See
	// notes/build-identity.md.
	Hostname     string `json:"hostname"`
	IP           string `json:"ip"`
	MachineID    string `json:"machine_id"`
	GitUserName  string `json:"git_user_name"`
	GitUserEmail string `json:"git_user_email"`
	User         string `json:"user"`

	// Raw so any payload shape decodes (the bbclass sends an object here); we
	// don't inspect it yet.
	Dump json.RawMessage `json:"dump"`

	Type     string          `json:"type"`
	Metadata json.RawMessage `json:"metadata"`
}

// sstateMeta is the metadata payload of a MissedSstate MetadataEvent. Each
// entry is [fn, task, hash, sstatefile]; we only count them for now.
type sstateMeta struct {
	Missed [][]any `json:"missed"`
	Found  [][]any `json:"found"`
}

func main() {
	addr := flag.String("addr", ":6768", "address the HTTP server listens on")
	dbPath := flag.String("db", "var/hashequiv/hashequiv.db", "path to the SQLite operational database")
	downloadsDir := flag.String("downloads", "var/downloads", "directory for the downloads (DL mirror) blob store")
	sstateDir := flag.String("sstate", "var/sstate", "directory for the sstate blob store")
	ledgerPath := flag.String("ledger", "var/yocache.ledger.jsonl", "path to the append-only audit ledger (created if absent)")
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

	ledger, err := openLedger(*ledgerPath, log)
	if err != nil {
		log.Error("cannot open ledger", "path", *ledgerPath, "err", err)
		os.Exit(1)
	}
	defer ledger.Close()
	log.Info("ledger ready", "path", *ledgerPath)

	// Blob stores for the two writable path spaces (DL mirror + sstate). Each
	// creates its dir and sweeps staging files left by an upload an earlier run
	// didn't finish; see upload.go for the dot-staging scheme. A bad path is
	// fatal — upload to that space would be permanently broken.
	downloads, err := newBlobUploader(*downloadsDir, "downloads", log, ledger)
	if err != nil {
		log.Error("cannot init downloads store", "err", err)
		os.Exit(1)
	}
	sstate, err := newBlobUploader(*sstateDir, "sstate", log, ledger)
	if err != nil {
		log.Error("cannot init sstate store", "err", err)
		os.Exit(1)
	}

	ver := buildVersionInfo()
	log.Info("yocache version", "version", ver.Version, "revision", ver.Revision, "modified", ver.Modified)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
	})
	mux.HandleFunc("GET /version", versionHandler(ver))

	// Hash-equivalence server, spoken over WebSocket on this same port. Point a
	// build at it with BB_HASHSERVE = "ws://host:6768/hashequiv". See
	// hashequiv.go for the protocol and the (thin, in-memory) store.
	mux.HandleFunc("/hashequiv", newHashEquiv(store, log, ledger).handle)

	// Build telemetry sink. yocache.bbclass POSTs one JSON report per
	// bitbake event. We just decode and log it for now — no persistence
	// yet; this proves the round trip and shows the real payload shape.
	mux.HandleFunc("POST /api/build-report", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil {
			log.Warn("build report: read failed", "err", err, "remote", r.RemoteAddr)
			http.Error(w, "read error", http.StatusBadRequest)
			return
		}
		var rep buildReport
		if err := json.Unmarshal(body, &rep); err != nil {
			log.Warn("build report: bad json", "err", err, "remote", r.RemoteAddr)
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}

		attrs := []any{
			"event", rep.Event,
			"build_name", rep.BuildName,
			"machine", rep.Machine,
			"distro", rep.Distro,
			"hostname", rep.Hostname,
			"ip", rep.IP,
			"user", rep.User,
		}
		// Provenance that's often absent (shared-uid container, no git config,
		// no host machine-id mounted) — only log what's present, to keep the
		// line readable. See notes/build-identity.md.
		if rep.MachineID != "" {
			attrs = append(attrs, "machine_id", rep.MachineID)
		}
		if rep.GitUserName != "" {
			attrs = append(attrs, "git_user_name", rep.GitUserName)
		}
		if rep.GitUserEmail != "" {
			attrs = append(attrs, "git_user_email", rep.GitUserEmail)
		}
		if rep.Type != "" {
			attrs = append(attrs, "type", rep.Type)
		}
		// Only MissedSstate carries the missed/found shape; for other
		// MetadataEvents the unmarshal just yields zero counts, which we skip.
		if len(rep.Metadata) > 0 {
			var s sstateMeta
			if err := json.Unmarshal(rep.Metadata, &s); err != nil {
				log.Warn("build report: bad metadata", "err", err, "type", rep.Type, "remote", r.RemoteAddr)
			} else if len(s.Missed) > 0 || len(s.Found) > 0 {
				attrs = append(attrs, "missed", len(s.Missed), "found", len(s.Found))
			}
		}
		attrs = append(attrs, "remote", r.RemoteAddr)
		log.Info("build report", attrs...)
		w.WriteHeader(http.StatusNoContent)
	})

	// Artifact cache: the build PUTs blobs (build-side uploader) and bitbake's
	// SSTATE_MIRRORS / PREMIRRORS GET them back. The GET pattern also serves HEAD
	// (bitbake HEADs an sstate object before fetching). Method-typed patterns are
	// more specific than the catch-all "/", so they win for these paths; a GET
	// that finds nothing returns 404, the same void outcome as before, so a miss
	// still falls back to upstream.
	//
	// Serving a stored object is unconditionally correct. The sstate "claim vs
	// available" gating from notes/sstate-upload-hook.md lives on the hashequiv
	// side (don't advertise a unihash until its object has landed), not here —
	// follow-up, built on this read side rather than blocking it.
	mux.HandleFunc("PUT /sstate/", sstate.put)
	mux.HandleFunc("PUT /downloads/", downloads.put)
	mux.HandleFunc("GET /sstate/", sstate.get)
	mux.HandleFunc("GET /downloads/", downloads.get)

	// Any method on the blob spaces that is not GET/HEAD/PUT has no defined
	// semantics — tell the caller explicitly rather than silently 404ing.
	methodNotAllowed := func(w http.ResponseWriter, r *http.Request) {
		log.Warn("method not allowed",
			"method", r.Method,
			"path", r.URL.Path,
			"remote", r.RemoteAddr,
		)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
	mux.HandleFunc("/sstate/", methodNotAllowed)
	mux.HandleFunc("/downloads/", methodNotAllowed)

	// Catch-all: anything not matched above is an unrecognised path.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Info("unmatched request",
			"method", r.Method,
			"path", r.URL.Path,
			"query", r.URL.RawQuery,
			"ua", r.UserAgent(),
			"remote", r.RemoteAddr,
		)
		http.Error(w, "not found", http.StatusNotFound)
	})

	srv := &http.Server{
		Addr: *addr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Server", "yocache/"+ver.Version)
			mux.ServeHTTP(w, r)
		}),
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
