# Changelog

All notable changes to this project are documented here. Format loosely
follows [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).

## Unreleased

## v0.1.11 - 2026-08-14

### Fixed
- Two clients uploading the same blob at the same moment no longer
  double-count against the quota; the storage limit stays accurate under
  concurrent pushes.

### Changed
- `README.md` is now aimed at people deploying and using YoCache rather than
  developing it: what it is, a three-step quick start covering both the
  container and the binary, and links into [yocache.dev](https://yocache.dev).
  Building from source, the devcontainer setup, editor configuration and the
  project conventions move to a new `CONTRIBUTING.md`, which GitHub surfaces
  in its own repository tab and sidebar entry.

### Added
- Web dashboard served by the yocache binary at `/ui/` (root `/`
  redirects there). Shows storage totals for downloads, sstate, and
  hash-equivalence, with a pie chart of blob-store byte share. Fully
  self-contained — no external hosting needed, no runtime dependency on
  Node; the SPA is compiled at build time and embedded via `//go:embed`.
  Assets go gzip-compressed on the wire — mainly for the UI, but JSON
  API responses benefit too.
- The Docker Hub repository page is now populated automatically. A new
  `Docker Hub description` workflow renders it from the documentation site,
  so nothing on it is maintained twice: the container instructions come from
  `site/src/content/docs/docker.md`, and the full settings table is lifted
  from `site/src/content/docs/server-configuration.md` and reshaped to lead
  with the environment variable, since nobody reading a registry page can
  pass a CLI flag. Registry-only framing lives in `.github/registry/`, and
  the result is PATCHed to Docker Hub whenever any of those change. Relative
  links are rewritten to absolute `yocache.dev` URLs; the render fails rather
  than publishing a link that would 404 off-site, or a settings table whose
  columns have moved.
- Container images now carry the standard OCI metadata labels
  (`title`, `url`, `documentation`, `source`, `licenses`, plus `version`,
  `revision`, `created` and `description` stamped at release time).
  `image.source` is what links the image to this repository on GHCR, which is
  how that package page picks up a README — GHCR has no per-package README,
  it renders the linked repository's. `image.description` is taken from the
  same `.github/registry/short-description.txt` as the Docker Hub short
  description, so the two registries can't disagree.

## v0.1.10 - 2026-08-08

### Changed
- Documentation site moves from `bmoczulski.github.io/yocache` to
  [yocache.dev](https://yocache.dev). Old GitHub Pages URLs redirect
  automatically.

### Added
- `YOCACHE_BLOCK_RECIPES` now propagates downstream: a recipe that
  transitively `DEPENDS` on a blocked recipe is skipped too, using
  `BB_TASKDEPDATA` (bitbake's own recursive per-task dependency closure) to
  find it — no separate graph walk needed. A blocked recipe's own build
  output can be non-deterministic in ways its taskhash never reflects, so a
  downstream consumer's sstate can silently inherit that same problem even
  though its own taskhash looks stable across machines. Both the directly
  blocked recipe and any downstream recipe skipped because of it now log a
  `bb.warn` (previously `bb.note` for the direct case) naming the
  responsible upstream recipe(s).

## v0.1.9 - 2026-07-19

### Removed
- `yocache.bbclass` no longer POSTs a per-event build report to the server,
  nor logs it locally — the server never persisted any of it (every payload
  was decoded and discarded), so it was pure overhead: hundreds to thousands
  of synchronous requests per build for no benefit. `INHERIT += "toaster"` and
  `INHERIT += "buildhistory"` are also dropped from the recommended setup,
  since their only purpose here was unlocking two of the now-removed events
  (`MissedSstate`, `TaskArtifacts`). Removed variables: `YOCACHE_SKIP_POST`,
  `YOCACHE_REPORT_ENDPOINT`, `YOCACHE_LOG`, `YOCACHE_LOG_LIMIT`,
  `YOCACHE_EVENTS`, `YOCACHE_METADATA_TYPES`. The server's
  `POST /api/build-report` endpoint stays, unused, for a possible future
  reinstatement with a real consumer. Mirror wiring and artifact upload are
  unaffected.
- `YOCACHE_SKIP_UPLOAD`, the blanket dry-run flag, is gone — it was fully
  redundant with `YOCACHE_SKIP_UPLOAD_TYPES = "all"`, which already produces
  the same "log what would be uploaded, send nothing" behavior.

### Added
- Hash-equivalence now dedups across different taskhashes that produce the
  same task output (cross-output equivalence): a `report` whose outhash
  matches an earlier taskhash's outhash unifies onto that taskhash's unihash,
  mirroring bitbake's own hashserv (`get_equivalent_for_outhash`). Previously
  every taskhash got its own unihash even when two machines' inputs differed
  but their actual output matched, missing sstate reuse bitbake's reference
  server would have found.
- LRU eviction now evicts an sstate task's archive together with its
  `.siginfo`/`.sig` sidecars as one unit, ordered by whichever sibling was
  most recently accessed — a cold sidecar can no longer get evicted alone
  and strand (or orphan) its still-cached archive. Once a group is fully
  evicted, its hash-equivalence entries are deleted too, so a unihash never
  outlives the blob it points at.

## v0.1.8 - 2026-07-18

### Added
- Multi-arch (linux/amd64, linux/arm64) Docker image, built and pushed on
  every release to GHCR (`ghcr.io/bmoczulski/yocache`) and Docker Hub
  (`docker.io/moczulski/yocache`). Runs as a non-root user by default, self-heals
  bind-mount ownership on the data volume when started as root, and honors an
  explicit `--user uid:gid` override.

## v0.1.7 - 2026-07-18

### Added
- Project website under `site/` (Astro + Starlight): landing page plus
  user-facing docs — getting started, why YoCache, server & client
  configuration, FAQ. Published to GitHub Pages on every push to `main` that
  touches `site/`, independent of versioned releases.
- Every server flag can now be set via a `YOCACHE_<FLAG>` environment
  variable (e.g. `YOCACHE_DATA_DIR`, `YOCACHE_ADDR`), with CLI flags still
  taking precedence over env vars over compiled-in defaults — the config
  path Docker and systemd deployments expect.
- `--hashequiv-addr` (default `:6767`, on by default) opens a second,
  dedicated raw-TCP listener speaking bitbake's legacy OEHASHEQUIV wire
  protocol directly (no WebSocket), for pre-Scarthgap bitbake (Dunfell and
  earlier) whose hash-equivalence client can't speak `ws://`. Point
  `BB_HASHSERVE` at `host:6767` to use it instead of
  `ws://host:6768/hashequiv`; pass `--hashequiv-addr ""` to disable it.
- `GET /api/stats` (and the startup "cache inventory" log line) now report
  hash-equivalence store size: `hashequiv_taskhashes` (recorded
  taskhash->unihash mappings), `hashequiv_unihashes` (distinct unihashes,
  the dedup signal), and `hashequiv_outhashes` (recorded outhash records).
- `meta-yocache` now declares every Yocto release from `dunfell` through
  `wrynose` in `LAYERSERIES_COMPAT_yocache`, closing the gap between the
  previously-declared dunfell/kirkstone/wrynose.

### Changed
- **Breaking:** the five path flags (`--db`, `--downloads`, `--sstate`,
  `--ledger`, `--access-log`) are replaced by a single `--data-dir` (default
  `var`, same on-disk layout underneath: `yocache.db`, `downloads/`, `sstate/`,
  `yocache.ledger.jsonl`, `yocache.access.jsonl`). Anything passing the old
  flags needs to switch to `--data-dir` pointing at one root — this also
  clears the way for a single Docker volume mount instead of five.

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
