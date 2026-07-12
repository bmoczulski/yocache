# Changelog

All notable changes to this project are documented here. Format loosely
follows [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).

## Unreleased

### Added
- `GET /api/stats` — JSON cache inventory summary (file counts, deduplicated
  sstate recipe count, cumulative size per category), computed live from the
  inventory DB so it can be polled at will instead of only at startup.

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
