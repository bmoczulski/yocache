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
// disk, the quota counter, and the inventory atomically per group (see
// blobGroup) — never per individual file, so an sstate archive and its
// .siginfo/.sig sidecars always leave together.
//
// If kind is set, eviction is scoped to that store kind only (e.g. "sstate")
// — blobs of other kinds are never considered, evicted, or touched. This
// backs the "lru-sstate" policy: sstate accumulates stale, no-longer-needed
// entries as a project evolves and should be trimmed first, while downloads
// are a flatter, saturating cost that's worth preserving longer.
//
// hashEquiv is nil-safe: when set, a fully-evicted sstate group also drops
// its unihashes/outhashes rows (see hashEquivStore.DeleteByUnihash), so a
// hash-equiv answer never outlives the blob it points at.
type LRUPolicy struct {
	inventory *blobInventory
	stores    map[string]string // kind → abs root dir
	quota     *quotaTracker
	ledger    *Ledger
	hashEquiv *hashEquivStore
	log       *slog.Logger
	kind      string // empty = all kinds
}

func (p *LRUPolicy) Name() string {
	if p.kind == "" {
		return "lru"
	}
	return "lru-" + p.kind
}

// Evict removes the oldest groups (by their most-recently-accessed member)
// until freed >= needed or the store is empty. Blobs in the inventory that no
// longer exist on disk are cleaned up silently — they represent an external
// delete and don't count toward freed (the space was already returned to the
// OS).
func (p *LRUPolicy) Evict(needed int64) (int64, error) {
	const batchSize = 50
	var freed int64
	for freed < needed {
		var cands []blobGroup
		var err error
		if p.kind == "" {
			cands, err = p.inventory.LRUGroupCandidates(batchSize)
		} else {
			cands, err = p.inventory.LRUGroupCandidatesByKind(p.kind, batchSize)
		}
		if err != nil {
			return freed, fmt.Errorf("lru evict: %w", err)
		}
		if len(cands) == 0 {
			break
		}
		for _, g := range cands {
			if freed >= needed {
				break
			}
			freed += p.evictGroup(g)
		}
		if len(cands) < batchSize {
			break // fewer than a full batch → store exhausted
		}
	}
	return freed, nil
}

// evictGroup removes every member of a group as one unit. Only once every
// member is confirmed gone (removed now, or already gone via an external
// delete) does it release quota, record ledger entries, and — for sstate —
// drop the matching hash-equiv rows: a member stranded by a real removal
// error means the group isn't actually evicted yet, so leaving both the
// quota accounting and the equivalence entry alone is the safe default,
// retried on a future pass.
func (p *LRUPolicy) evictGroup(g blobGroup) int64 {
	dir, ok := p.stores[g.Kind]
	if !ok {
		// Stale inventory entries for a store kind we no longer manage.
		for _, m := range g.Members {
			_ = p.inventory.Remove(m.Kind, m.Path)
		}
		return 0
	}

	var freed int64
	clear := true
	for _, m := range g.Members {
		full := filepath.Join(dir, m.Path)
		if err := os.Remove(full); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				p.log.Warn("lru evict: remove failed", "path", full, "err", err)
				clear = false
				continue
			}
			// File was deleted externally — clean the stale inventory record
			// but do not count as freed: the OS already reclaimed the space,
			// and we didn't free it in this run.
			if err := p.inventory.Remove(m.Kind, m.Path); err != nil {
				p.log.Warn("lru evict: inventory remove failed", "kind", m.Kind, "path", m.Path, "err", err)
			}
			continue
		}
		if err := p.inventory.Remove(m.Kind, m.Path); err != nil {
			p.log.Warn("lru evict: inventory remove failed", "kind", m.Kind, "path", m.Path, "err", err)
		}
		p.quota.release(m.Size)
		p.ledger.RecordArtifactEvicted(m.Kind, m.Path, p.Name(), "")
		freed += m.Size
	}

	if clear && g.Kind == "sstate" && p.hashEquiv != nil && len(g.Members) > 0 {
		if checksum := sstateChecksum(g.Members[0].Path); checksum != "" {
			if _, _, err := p.hashEquiv.DeleteByUnihash(checksum); err != nil {
				p.log.Warn("lru evict: hash-equiv cleanup failed", "checksum", checksum, "err", err)
			}
		}
	}

	return freed
}
