# Changelog

All notable changes to this project are documented here. Format loosely
follows [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).

## Unreleased

## v0.1.6 - 2026-07-14

### Fixed
- sstate build-time attribution now credits upstream, non-sstate-cacheable
  tasks (`do_fetch`/`do_unpack`/`do_patch`/`do_configure`/`do_compile`/
  `do_install`) to whichever downstream sstate object also lets bitbake skip
  them, instead of only reporting that object's own (typically sub-second)
  packaging time. Previously a compile-heavy recipe's reported "time saved"
  could be off by orders of magnitude, since `do_compile` itself is never a
  cache-eligible task in a typical build.

## v0.1.5 - 2026-07-13

### Added
- `meta-yocache` now declares `kirkstone` in `LAYERSERIES_COMPAT_yocache`.

## v0.1.4 - 2026-07-13

### Fixed
- Build-end cache-benefit summary line no longer pads in a misleading
  "0 download object(s)" / "saving ~00:00:00" clause for a side (reused or
  contributed) the build never actually touched.

## v0.1.3 - 2026-07-12

### Added
- `GET /api/stats` — JSON cache inventory summary (file counts, deduplicated
  sstate recipe count, cumulative size per category), computed live from the
  inventory DB so it can be polled at will instead of only at startup.
- `GET /api/build-stats/{buildname}` — per-build cache-benefit summary: bytes
  uploaded/downloaded and, for sstate, the build time contributed and saved
  by reusing cache instead of rebuilding. yocache.bbclass now captures each
  sstate task's wall-clock build time and uploads it alongside the artifact,
  and prints a one-line "yocache helped you / you helped your teammates"
  summary at the end of every build.
- `--build-stats-ttl` (default `720h`, ~1 month) controls how long per-build
  download stats are retained before an in-process daily garbage collection
  sweep prunes them.

## v0.1.2 - 2026-07-12

### Fixed
- Release workflow now actually publishes the curated `CHANGELOG.md` section
  as GitHub Release notes. `changelog.disable: true` in `.goreleaser.yaml` was
  skipping the whole changelog/release-notes pipe, so `--release-notes` was
  silently ignored and v0.1.1 shipped with an empty release body.

## v0.1.1 - 2026-07-12

### Fixed
- Release workflow no longer fails GoReleaser's git-dirty-state check by
  writing the generated release notes outside the checked-out repo.

## v0.1.0 - 2026-07-12

### Added
- Single-node blob cache for Yocto sstate and DL-mirror artifacts, with
  crash-safe staged uploads and atomic rename into place.
- Hash-equivalence server speaking bitbake's OEHASHEQUIV protocol over
  WebSocket (`/hashequiv`), SQLite-backed so unihashes survive a restart.
- Quota tracking with pluggable eviction policies (`--evict lru`,
  `--evict lru-sstate`).
- Recipe block list (`--block-recipe`) to reject cache ops for recipes known
  to produce broken sstate.
- Identity-prefixed URLs and a JSONL access log / ledger for build telemetry.
