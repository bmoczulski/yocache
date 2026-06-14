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
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/dustin/go-humanize"
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
	dbPath := flag.String("db", "var/yocache.db", "path to the SQLite operational database")
	downloadsDir := flag.String("downloads", "var/downloads", "directory for the downloads (DL mirror) blob store")
	sstateDir := flag.String("sstate", "var/sstate", "directory for the sstate blob store")
	quotaStr := flag.String("quota", "0", "total storage quota for all blob stores (e.g. 500MiB, 10GB); 0 means unlimited")
	ledgerPath := flag.String("ledger", "var/yocache.ledger.jsonl", "path to the mutation ledger: artifact.added, artifact.evicted (created if absent)")
	accessLogPath := flag.String("access-log", "var/yocache.access.jsonl", "path to the access log: artifact.fetched, artifact.missed (created if absent)")
	var evictPolicies []string
	flag.Func("evict", "eviction `policy` to enable (lru); repeat to chain policies in order", func(v string) error {
		evictPolicies = append(evictPolicies, v)
		return nil
	})
	var blockListRecipes []string
	flag.Func("block-recipe", "recipe `name` to block from all cache operations (GET/PUT/HEAD); repeat to add more", func(v string) error {
		blockListRecipes = append(blockListRecipes, v)
		return nil
	})
	flag.Parse()

	quotaUint, err := humanize.ParseBytes(*quotaStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --quota value %q: %v\n", *quotaStr, err)
		os.Exit(1)
	}
	quotaBytes := int64(quotaUint)

	blockList := newRecipeBlockList(blockListRecipes)

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if len(blockList) > 0 {
		log.Info("recipe block list active", "recipes", blockListRecipes)
	}

	// Operational state lives in a single SQLite file shared by all stores.
	// Make the parent dir so the default "var/" path works out of the box.
	if dir := filepath.Dir(*dbPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Error("cannot create database directory", "dir", dir, "err", err)
			os.Exit(1)
		}
	}
	db, err := openOperationalDB(*dbPath)
	if err != nil {
		log.Error("cannot open operational db", "path", *dbPath, "err", err)
		os.Exit(1)
	}
	defer db.Close()
	if err := migrateDB(db); err != nil {
		log.Error("cannot migrate operational db", "path", *dbPath, "err", err)
		os.Exit(1)
	}
	log.Info("operational db ready", "path", *dbPath)
	store := &hashEquivStore{db: db}
	inv := openBlobInventory(db)

	ledger, err := openLedger(*ledgerPath, log)
	if err != nil {
		log.Error("cannot open ledger", "path", *ledgerPath, "err", err)
		os.Exit(1)
	}
	defer ledger.Close()
	log.Info("ledger ready", "path", *ledgerPath)

	accessLog, err := openLedger(*accessLogPath, log)
	if err != nil {
		log.Error("cannot open access log", "path", *accessLogPath, "err", err)
		os.Exit(1)
	}
	defer accessLog.Close()
	log.Info("access log ready", "path", *accessLogPath)

	// Blob stores for the two writable path spaces (DL mirror + sstate). Each
	// creates its dir and wipes the .uploads staging subtree left by an earlier
	// run; see upload.go for the staging scheme. A bad path is fatal — upload to
	// that space would be permanently broken.
	qt := &quotaTracker{limit: quotaBytes}
	downloads, err := newBlobUploader(*downloadsDir, "downloads", log, ledger, accessLog, qt, inv, nil)
	if err != nil {
		log.Error("cannot init downloads store", "err", err)
		os.Exit(1)
	}
	sstate, err := newBlobUploader(*sstateDir, "sstate", log, ledger, accessLog, qt, inv, nil)
	if err != nil {
		log.Error("cannot init sstate store", "err", err)
		os.Exit(1)
	}
	qt.seed(*downloadsDir, *sstateDir)
	if qt.limit > 0 {
		log.Info("storage quota active", "limit_bytes", qt.limit, "used_bytes", qt.Used())
	} else {
		log.Info("storage quota disabled (unlimited)")
	}

	// Build the eviction manager from --evict flags.
	stores := map[string]string{"downloads": *downloadsDir, "sstate": *sstateDir}
	var policies []EvictionPolicy
	for _, name := range evictPolicies {
		switch name {
		case "lru":
			policies = append(policies, &LRUPolicy{
				inventory: inv,
				stores:    stores,
				quota:     qt,
				ledger:    ledger,
				log:       log,
			})
		default:
			log.Error("unknown eviction policy", "policy", name)
			os.Exit(1)
		}
	}
	var evMgr *EvictionManager
	if len(policies) > 0 {
		evMgr = &EvictionManager{policies: policies, log: log}
		log.Info("eviction enabled", "policies", evictPolicies)

		// Seed the inventory with blobs already on disk so the eviction order
		// reflects reality from the first upload, not just post-restart arrivals.
		if err := inv.Retrofit(stores); err != nil {
			log.Error("inventory retrofit failed", "err", err)
			os.Exit(1)
		}
		log.Info("inventory retrofit complete")
	}

	// Wire the eviction manager into the uploaders now that it's built.
	downloads.eviction = evMgr
	sstate.eviction = evMgr

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

	// Blob spaces: all methods handled in one place.
	//
	// GET / HEAD: both direct (/sstate/<blob>) and identity-prefixed
	// (/machine/<m>/buildname/<b>/sstate/<blob>) URLs are accepted so the access
	// log captures whatever context the bbclass embedded in the mirror URL.
	// http.ServeContent (called inside serveBlob) handles HEAD correctly —
	// headers only, no body.
	//
	// PUT: uploader.py always sends direct /sstate/<blob> paths (identity travels
	// in X-BitBake-var-* headers); parseIdentityPath handles it correctly
	// (empty identity map, kind and blob extracted as normal).
	//
	// Serving a stored object is unconditionally correct. The sstate "claim vs
	// available" gating from notes/sstate-upload-hook.md lives on the hashequiv
	// side (don't advertise a unihash until its object has landed), not here.
	blobStores := map[string]*blobUploader{"sstate": sstate, "downloads": downloads}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		identity, kind, blobName, ok := parseIdentityPath(r.URL.Path)
		if ok && blockList.blocked(kind, blobName) {
			log.Warn("blocked recipe",
				"kind", kind, "name", blobName, "method", r.Method, "remote", r.RemoteAddr)
			http.Error(w, "recipe blocked", http.StatusForbidden)
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			if !ok {
				log.Info("unmatched request",
					"method", r.Method, "path", r.URL.Path,
					"query", r.URL.RawQuery, "ua", r.UserAgent(), "remote", r.RemoteAddr,
				)
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			blobStores[kind].serveBlob(w, r, blobName, identity["machine"], identity["buildname"])
		case http.MethodPut:
			if !ok {
				log.Info("unmatched request",
					"method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr,
				)
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			blobStores[kind].put(w, r)
		default:
			if ok {
				log.Warn("method not allowed",
					"method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr,
				)
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			} else {
				log.Info("unmatched request",
					"method", r.Method, "path", r.URL.Path,
					"query", r.URL.RawQuery, "ua", r.UserAgent(), "remote", r.RemoteAddr,
				)
				http.Error(w, "not found", http.StatusNotFound)
			}
		}
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
