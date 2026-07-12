# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`yocache` is a smart, writable cache server for Yocto/bitbake build farms: a
shared **DL mirror + sstate cache** with built-in **hash-equivalence**, and
mDNS discovery + peer federation planned. A single-node cache now works
end-to-end — bitbake uploads artifacts on a cache miss and every machine fetches
them back. `notes/concept.md` is the long-form design dialogue and the
authoritative source for *why* decisions were made; read it before proposing
architectural changes.

Status: single-node sstate+DL+hash-equiv is functional. Not yet built: mDNS/
DNS-SD discovery (`_yocache._tcp`), peer gossip + pull, and full federation.
Build order is single-node → mDNS → peer pull → federation.

## The two halves

The repo contains both sides of the system, which communicate **over HTTP and
WebSocket on port 6768**:

- **Server** — the Go binary under [cmd/yocache/](cmd/yocache/). Endpoints:
  - `GET /healthz`, `GET /version` — liveness + build stamp.
  - `POST /api/build-report` — decodes-and-logs one JSON telemetry payload per
    bitbake event (no persistence yet; proves the round trip and shows payload
    shape). The Go `buildReport` struct in [main.go](cmd/yocache/main.go)
    mirrors the bbclass JSON by hand.
  - `/hashequiv` — bitbake hash-equivalence protocol over **WebSocket**
    ([hashequiv.go](cmd/yocache/hashequiv.go) + SQLite-backed
    [hashequiv_store.go](cmd/yocache/hashequiv_store.go)).
  - catch-all `/` — the blob store ([upload.go](cmd/yocache/upload.go)),
    handling `GET`/`HEAD` (serve or 404-miss so bitbake falls back to upstream)
    and `PUT` (upload) for both `/sstate/…` and `/downloads/…`. This is the
    headline feature: **bitbake has no built-in sstate/DL upload path**, which is
    the gap this project bridges.
- **Build-side layer** — [meta-yocache/](meta-yocache/), enabled with
  `INHERIT += "yocache"`. Two jobs:
  - [classes/yocache.bbclass](meta-yocache/classes/yocache.bbclass) subscribes to
    a curated set of bitbake events and POSTs one JSON payload per event to
    `/api/build-report` (also appended to a JSONL log, `YOCACHE_LOG`). It also
    wires the mirrors: it prepends `PREMIRRORS` and `SSTATE_MIRRORS` from
    `YOCACHE_URL` (see the mirror `.inc` files —
    [yocache-mirrors-new.inc](meta-yocache/classes/yocache-mirrors-new.inc) and
    the compat variant — chosen by the running bitbake's capabilities).
  - [lib/yocache/uploader.py](meta-yocache/lib/yocache/uploader.py) is a
    cooker-resident uploader thread. `SSTATEPOSTCREATEFUNCS` /
    `do_fetch[postfuncs]` hooks notify it over a unix socket, and it `PUT`s each
    blob to the server. See [notes/sstate-upload-hook.md](notes/sstate-upload-hook.md).

The bbclass header comment documents exactly which events carry cache signal and
why others are excluded, and how build provenance (hostname/IP/machine-id/git
identity) is gathered out-of-band — preserve that rationale when editing. If you
change the report shape on one side, change the mirroring `buildReport` struct on
the other.

## Storage & persistence

- **Blobs** live on the filesystem: `var/downloads` and `var/sstate` (flags
  `--downloads` / `--sstate`). Uploads are crash- and reader-safe: the body is
  streamed to a private `.uploads/<token>/` staging dir and atomically
  `rename(2)`d into place only after a full, fsync'd write. `PUT` requires
  `If-None-Match` and `Content-Length`; a same-size existing blob is skipped
  (`412`), a size mismatch is a `409` conflict — except growing VCS mirror
  tarballs (`git2_`/`gitshallow_`/`hg_`/`repo_`), which are allowed to replace a
  smaller stored snapshot.
- **Operational state** is a single **SQLite (WAL)** database (`--db`,
  default `var/yocache.db`) shared by all stores. Schema lives in
  [cmd/yocache/migrations/](cmd/yocache/migrations/) and is applied at startup by
  goose (`//go:embed`). Tables: `unihashes`/`outhashes` (hash-equiv) and `blobs`
  (per-blob size + `accessed_at`, the source of truth for eviction order).
- **Quota + eviction**: `--quota` (e.g. `500MiB`, `0` = unlimited) caps total
  blob bytes; `--evict lru` (repeatable to chain policies) frees space on demand
  by removing least-recently-accessed blobs. See
  [upload.go](cmd/yocache/upload.go) (`quotaTracker`),
  [eviction.go](cmd/yocache/eviction.go), and
  [blob_inventory.go](cmd/yocache/blob_inventory.go).
- **Two JSONL audit logs** (append-only, jq/DuckDB-friendly — see
  [ledger.go](cmd/yocache/ledger.go)): the **ledger** (`--ledger`) records
  server state mutations (`artifact.added`, `artifact.evicted`, `quota.exceeded`,
  `hash.equiv_set`); the **access log** (`--access-log`) records
  `artifact.fetched` / `artifact.missed`. Both are drained by a dedicated
  goroutine so a slow/full log never stalls a request.
- **DuckDB** is used only for offline analytics over the JSONL logs (the
  `scripts/` below), not as a live store.

Planned but not yet built: mDNS/DNS-SD discovery, peer gossip/pull, federation.

## Identity-prefixed URLs & the recipe block list

GET/HEAD accept both direct (`/sstate/<blob>`) and identity-prefixed
(`/machine/<m>/buildname/<b>/sstate/<blob>`) URLs so the access log captures which
build/machine issued the lookup; `parseIdentityPath` extracts it. PUT sends
direct paths and carries identity in `X-BitBake-var-*` headers instead. The
`--block-recipe <PN>` flag (repeatable) rejects all cache ops for a recipe known
to produce broken sstate ([blocklist.go](cmd/yocache/blocklist.go)); it matches
the `sstate:<PN>:…` filename pattern and never affects downloads.

## Development

There is **no local Go toolchain**; everything runs in the container defined in
[.devcontainer/Dockerfile](.devcontainer/Dockerfile) (Go 1.26). The container
**must run as the host uid/gid** so bind-mounted files aren't root-owned and
git's `.git` ownership check passes (which keeps `go build` VCS stamping working).
Use a rootless engine, or pass `--user` explicitly with rootful Docker.

Helper scripts wrap the container invocations (rootless Podman by default):

```sh
./build.sh   # go build + go vet + go test -race, all ./cmd/... inside the container
./shell.sh   # interactive shell in the toolchain container
./serve.sh   # run the locally-built ./yocache binary (--evict lru)
```

Or drive the container directly (for rootful Docker swap `--userns=keep-id` for
`--user "$(id -u):$(id -g)"`):

```sh
docker build -t yocache-dev .devcontainer            # build toolchain image once
podman run --rm -it --userns=keep-id -v "$PWD":/workspace -w /workspace yocache-dev go build ./...
podman run --rm -it --userns=keep-id -p 6768:6768 -v "$PWD":/workspace -w /workspace yocache-dev \
  go run ./cmd/yocache --addr :6768
```

Module/build caches persist under `./.cache` (git-ignored), so rebuilds are fast.
Tests live alongside the code (`*_test.go` in [cmd/yocache/](cmd/yocache/)); run a
single one with `go test -race -run TestName ./cmd/yocache`.

## Exercising the cache against a real build

[testdata/yocto/](testdata/yocto/) is a reproducible kas-based Yocto build used
to drive real traffic at the server. **Run the Yocto build via `kas-container` on
the host, not inside the Go devcontainer** — yocache runs separately and they
talk only over HTTP. See [testdata/yocto/README.md](testdata/yocto/README.md) for
the full flow. For fast turnaround,
[notes/cheap-build-targets.md](notes/cheap-build-targets.md) lists small
`*-native` targets (e.g. `quilt-native`) that fire useful event batches in
seconds instead of building a whole image.

### Running ad-hoc commands inside the build environment

To exercise yocache against bitbake's *own* client code (e.g. the hash-equiv
protocol) without a full build, run a one-off command in the kas-container, which
already has bitbake's libs on `PYTHONPATH` and `websockets` installed. From
`testdata/yocto/example-project`:

```sh
KAS_IMAGE_VERSION=5.2 KAS_CONTAINER_ENGINE=podman \
  ../bin/kas-container-5.2 --runtime-args "--network=host" \
  shell yocache.yml -c "python3 /work/_some_script.py"
```

`--network=host` lets the command reach a yocache server bound on the host at
`localhost:6768`. Path mapping inside the container: the kasfile's git repo (the
yocache repo root) is `/repo`, and `KAS_WORK_DIR` (i.e. `example-project`, the dir
you run from) is `/work` — so `testdata/yocto/example-project/foo.py` is
`/work/foo.py` inside. `import hashserv` / `import bb` resolve from the sourced
bitbake env (fallback: `/work/bitbake/lib`).

### Hash-equivalence (`BB_HASHSERVE` over WebSocket)

yocache speaks bitbake's hash-equivalence protocol ("OEHASHEQUIV" over
`bb.asyncrpc`) natively over **WebSocket**, on the same port 6768, at
`/hashequiv`. Point a build at it from `local.conf`/`site.conf` (not the bbclass —
cooker reads `BB_HASHSERVE`, a per-recipe class can't set it). WebSocket
hash-equiv needs Yocto ≥ Scarthgap:

```
BB_HASHSERVE = "ws://localhost:6768/hashequiv"
```

The store is **SQLite-backed** (survives restart, which matters — a shifting
unihash across a restart changes dependent taskhashes and trips bitbake's
StaleSetSceneTasks). It is still **first-write-wins with no cross-output
equivalence dedup** yet (a reported `outhash` never unifies two different
taskhashes); output-based equivalence is a follow-up.

### Analyzing captured telemetry

```sh
scripts/summarize-builds.sh [path/to/yocache-events.jsonl]   # one row per build
scripts/summarize-events.sh [path/to/yocache-events.jsonl]   # counts per event type
```

`notes/` holds source-verified references (bitbake event catalogue, git mirror
tarball shapes, build identity, sstate upload hook) backing the design — consult
them before guessing at bitbake behaviour.

## Release automation

Every push to `main` that changes shippable content (i.e. not docs/notes-only)
is auto-released: [.github/workflows/release.yml](.github/workflows/release.yml)
runs the same test suite as `build.sh`, then cuts `CHANGELOG.md`'s `Unreleased`
section into a new version, tags it, and publishes a GitHub Release — binaries
for linux/darwin × amd64/arm64 via [.goreleaser.yaml](.goreleaser.yaml) (no cgo
needed, `modernc.org/sqlite` is pure Go), plus a tarball/zip of
[meta-yocache/](meta-yocache/). [VERSION](VERSION) holds `MAJOR.MINOR`; only the
patch auto-increments — bump `MAJOR`/`MINOR` yourself by editing that file.
[.github/workflows/ci.yml](.github/workflows/ci.yml) covers PRs/branches with no
release side effects. The release job fails outright if `CHANGELOG.md` has no
`Unreleased` entries, so see the convention below.

To validate `.goreleaser.yaml` or dry-run a build locally without waiting on
CI, use the gitignored `./goreleaser` wrapper (runs the official
`goreleaser/goreleaser` Docker image — not a repo script, recreate it if
missing: `./goreleaser check` or `./goreleaser release --snapshot --clean`).

## Conventions

- **Telemetry must never break a build.** In the bbclass, every network/parse
  failure is caught and downgraded to `bb.warn`; keep it that way. Likewise on
  the server, ledger/inventory failures are logged, never fatal to a request.
- Default port is **6768** throughout (binary, bbclass, mDNS service name).
- The bbclass is deliberately scaffolding: synchronous per-event POSTs mean
  hundreds-to-thousands of requests per build. Batching is required before this
  is enabled on large builds — don't treat the current shape as the target.
- Commits: Conventional Commits, no emojis, no `Co-authored-by`; keep them small
  and surgical (see the global instructions).
- **Every user-facing change needs a `CHANGELOG.md` entry** under `##
  Unreleased`, in the same commit/PR as the change — this is what the release
  workflow turns into GitHub Release notes (see "Release automation" above). A
  push to `main` with no new entry fails the release job outright.
