package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// EvictionPolicy frees disk space on demand. A policy removes blobs according
// to its own ordering (LRU, LFU, manual, …) and reports how many bytes it
// actually freed. Freed may be less than needed when the store is nearly empty.
type EvictionPolicy interface {
	Name() string
	Evict(needed int64) (freed int64, err error)
}

// EvictionManager chains one or more EvictionPolicy implementations. TryFree
// walks the chain until freed >= needed or all policies are exhausted.
// A nil manager or one with no policies is a safe no-op.
type EvictionManager struct {
	policies []EvictionPolicy
	log      *slog.Logger
}

// TryFree attempts to free at least needed bytes. It calls each policy in
// order, stopping as soon as the cumulative freed bytes reaches needed.
// Returns total freed bytes (may be less than needed if the store is too empty)
// and any errors collected from the policies. Policy errors are non-fatal: the
// chain continues past a failing policy so partial eviction is still attempted.
func (m *EvictionManager) TryFree(needed int64) (int64, error) {
	if m == nil || len(m.policies) == 0 {
		return 0, nil
	}
	var total int64
	var errs []error
	for _, p := range m.policies {
		if total >= needed {
			break
		}
		freed, err := p.Evict(needed - total)
		if err != nil {
			m.log.Warn("eviction policy error", "policy", p.Name(), "err", err)
			errs = append(errs, err)
		}
		total += freed
	}
	return total, errors.Join(errs...)
}

// LRUPolicy evicts the least-recently-accessed blobs first. It uses
// blobInventory as the source of truth for ordering and removes blobs from
// disk, the quota counter, and the inventory atomically per blob.
//
// If kind is set, eviction is scoped to that store kind only (e.g. "sstate")
// — blobs of other kinds are never considered, evicted, or touched. This
// backs the "lru-sstate" policy: sstate accumulates stale, no-longer-needed
// entries as a project evolves and should be trimmed first, while downloads
// are a flatter, saturating cost that's worth preserving longer.
type LRUPolicy struct {
	inventory *blobInventory
	stores    map[string]string // kind → abs root dir
	quota     *quotaTracker
	ledger    *Ledger
	log       *slog.Logger
	kind      string // empty = all kinds
}

func (p *LRUPolicy) Name() string {
	if p.kind == "" {
		return "lru"
	}
	return "lru-" + p.kind
}

// Evict removes the oldest blobs (by accessed_at) until freed >= needed or the
// store is empty. Blobs in the inventory that no longer exist on disk are
// cleaned up silently — they represent an external delete and don't count
// toward freed (the space was already returned to the OS).
func (p *LRUPolicy) Evict(needed int64) (int64, error) {
	const batchSize = 50
	var freed int64
	for freed < needed {
		var cands []blobRecord
		var err error
		if p.kind == "" {
			cands, err = p.inventory.LRUCandidates(batchSize)
		} else {
			cands, err = p.inventory.LRUCandidatesByKind(p.kind, batchSize)
		}
		if err != nil {
			return freed, fmt.Errorf("lru evict: %w", err)
		}
		if len(cands) == 0 {
			break
		}
		for _, r := range cands {
			if freed >= needed {
				break
			}
			dir, ok := p.stores[r.Kind]
			if !ok {
				// Stale inventory entry for a store kind we no longer manage.
				_ = p.inventory.Remove(r.Kind, r.Path)
				continue
			}
			full := filepath.Join(dir, r.Path)
			if err := os.Remove(full); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					// File was deleted externally — clean the stale inventory
					// record but do not count as freed: the OS already reclaimed
					// the space, and we didn't free it in this run.
					_ = p.inventory.Remove(r.Kind, r.Path)
				} else {
					p.log.Warn("lru evict: remove failed", "path", full, "err", err)
				}
				continue
			}
			if err := p.inventory.Remove(r.Kind, r.Path); err != nil {
				p.log.Warn("lru evict: inventory remove failed",
					"kind", r.Kind, "path", r.Path, "err", err)
			}
			p.quota.release(r.Size)
			p.ledger.RecordArtifactEvicted(r.Kind, r.Path, p.Name(), "")
			freed += r.Size
		}
		if len(cands) < batchSize {
			break // fewer than a full batch → store exhausted
		}
	}
	return freed, nil
}
