package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"time"
)

// Ledger is an append-only JSONL audit log of important server-side state
// changes. It is distinct from the build telemetry log (YOCACHE_LOG on the
// bbclass side): that records what bitbake events happened during a build; this
// records what the server did to its own stored state.
//
// One JSON object per line; parseable with jq or DuckDB's read_ndjson(). New
// entry types are added by defining a new details struct and a new Record*
// method — no schema migration, no format change.
//
// A dedicated drain goroutine owns the file descriptor; handler goroutines
// send pre-marshaled lines to a buffered channel and return immediately.
// The channel absorbs bursts without blocking callers; if it is full the entry
// is dropped with a warning rather than stalling a build request.
// flock is not needed: yocache is a single binary; all writers share the same
// process. If multi-server federation ever needs a shared ledger, the right
// path is SQLite (already planned for inventory state), not flock over JSONL.
type Ledger struct {
	ch   chan []byte
	done chan struct{}
	f    *os.File
	log  *slog.Logger
}

// ledgerEntry is the on-wire shape of every ledger line.
type ledgerEntry struct {
	Ts      time.Time       `json:"ts"`
	Type    string          `json:"type"`
	BuildID string          `json:"build_id,omitempty"`
	Details json.RawMessage `json:"details,omitempty"`
}

// openLedger opens (or creates) the JSONL ledger file at path. The file is
// opened for append so existing history is never overwritten on restart.
func openLedger(path string, log *slog.Logger) (*Ledger, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	l := &Ledger{
		ch:   make(chan []byte, 4096),
		done: make(chan struct{}),
		f:    f,
		log:  log,
	}
	go l.drain()
	return l, nil
}

// drain is the sole writer to the file. It runs until ch is closed, then
// closes done so Close can unblock and shut down cleanly.
func (l *Ledger) drain() {
	defer close(l.done)
	for b := range l.ch {
		if _, err := l.f.Write(b); err != nil {
			l.log.Warn("ledger: write failed", "err", err)
		}
	}
}

// Close signals drain to stop, waits for it to flush all buffered entries,
// then closes the file. Must be called only after all Record* callers have
// returned (i.e. after the HTTP server has shut down).
func (l *Ledger) Close() error {
	close(l.ch)
	<-l.done
	return l.f.Close()
}

// write marshals entry to JSON and enqueues it for the drain goroutine.
// Errors are logged as warnings — a ledger failure must never stall or break
// a build request. If the channel is full the entry is dropped.
func (l *Ledger) write(entry ledgerEntry) {
	b, err := json.Marshal(entry)
	if err != nil {
		l.log.Warn("ledger: marshal failed", "type", entry.Type, "err", err)
		return
	}
	b = append(b, '\n')
	select {
	case l.ch <- b:
	default:
		l.log.Warn("ledger: channel full, entry dropped", "type", entry.Type)
	}
}

// marshalDetails is a helper that marshals a details value and returns a
// RawMessage. On failure it returns nil and logs — the entry is still written
// (without details) rather than dropped.
func (l *Ledger) marshalDetails(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		l.log.Warn("ledger: details marshal failed", "err", err)
		return nil
	}
	return b
}

// --- Entry types ---

// artifactAddedDetails is the details payload for artifact.added.
type artifactAddedDetails struct {
	Kind string `json:"kind"` // "sstate" or "downloads"
	Path string `json:"path"` // relative path inside the store
	Size int64  `json:"size"` // bytes written
}

// RecordArtifactAdded records that a blob was successfully stored (after a PUT
// committed). size is the number of bytes written to disk.
func (l *Ledger) RecordArtifactAdded(kind, path string, size int64, buildID string) {
	l.write(ledgerEntry{
		Ts:      time.Now().UTC(),
		Type:    "artifact.added",
		BuildID: buildID,
		Details: l.marshalDetails(artifactAddedDetails{Kind: kind, Path: path, Size: size}),
	})
}

// artifactFetchedDetails is the details payload for artifact.fetched.
type artifactFetchedDetails struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
}

// RecordArtifactFetched records that a stored blob was served to a client (a
// cache hit on GET).
func (l *Ledger) RecordArtifactFetched(kind, path, buildID string) {
	l.write(ledgerEntry{
		Ts:      time.Now().UTC(),
		Type:    "artifact.fetched",
		BuildID: buildID,
		Details: l.marshalDetails(artifactFetchedDetails{Kind: kind, Path: path}),
	})
}

// artifactMissedDetails is the details payload for artifact.missed.
type artifactMissedDetails struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
}

// RecordArtifactMissed records that a lookup found no stored blob (a cache
// miss on GET/HEAD — bitbake will fall back to the upstream mirror).
func (l *Ledger) RecordArtifactMissed(kind, path, buildID string) {
	l.write(ledgerEntry{
		Ts:      time.Now().UTC(),
		Type:    "artifact.missed",
		BuildID: buildID,
		Details: l.marshalDetails(artifactMissedDetails{Kind: kind, Path: path}),
	})
}

// artifactEvictedDetails is the details payload for artifact.evicted.
// Not yet called — defined here as the extension point for eviction policy.
type artifactEvictedDetails struct {
	Kind       string `json:"kind"`
	Path       string `json:"path"`
	Policy     string `json:"policy"`               // e.g. "lru", "lfu", "manual"
	EvictedFor string `json:"evicted_for,omitempty"` // artifact that triggered the eviction
}

// RecordArtifactEvicted records that a blob was removed by an eviction policy.
// Called by future eviction logic; not wired up yet.
func (l *Ledger) RecordArtifactEvicted(kind, path, policy, evictedFor string) {
	l.write(ledgerEntry{
		Ts:   time.Now().UTC(),
		Type: "artifact.evicted",
		Details: l.marshalDetails(artifactEvictedDetails{
			Kind: kind, Path: path, Policy: policy, EvictedFor: evictedFor,
		}),
	})
}

// hashEquivSetDetails is the details payload for hash.equiv_set.
type hashEquivSetDetails struct {
	Method   string `json:"method"`
	Taskhash string `json:"taskhash"`
	Unihash  string `json:"unihash"`
}

// RecordHashEquivSet records that the server assigned a new unihash for a
// (method, taskhash) pair (first-write-wins).
func (l *Ledger) RecordHashEquivSet(method, taskhash, unihash, buildID string) {
	l.write(ledgerEntry{
		Ts:      time.Now().UTC(),
		Type:    "hash.equiv_set",
		BuildID: buildID,
		Details: l.marshalDetails(hashEquivSetDetails{
			Method: method, Taskhash: taskhash, Unihash: unihash,
		}),
	})
}
