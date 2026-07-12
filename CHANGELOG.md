# Changelog

All notable changes to this project are documented here. Format loosely
follows [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).

## Unreleased

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
