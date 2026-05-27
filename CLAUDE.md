# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`yocache` is a smart, writable cache server for Yocto/bitbake build farms (DL
mirror + sstate cache, with mDNS discovery and federation planned). It is in an
**early scaffold** stage: the server is a "void cache" that observes traffic but
stores nothing yet. `concept.md` is the long-form design dialogue and the
authoritative source for *why* decisions were made; read it before proposing
architectural changes.

## The two halves

The repo contains both sides of the system, which communicate **over HTTP and
WebSocket on port 6768**:

- **Server** — [cmd/yocache/main.go](cmd/yocache/main.go), a single Go binary.
  Today it serves `GET /healthz`, decodes-and-logs `POST /api/build-report`, a
  catch-all `/` handler that logs every `/sstate/` and `/downloads/` lookup and
  returns 404 ("void cache" — bitbake then falls back to upstream so builds still
  complete), and a `/hashequiv` WebSocket endpoint speaking bitbake's
  hash-equivalence protocol ([cmd/yocache/hashequiv.go](cmd/yocache/hashequiv.go),
  thin in-memory store — see the "Hash-equivalence" section below). No blob
  persistence, no proxying, no upload yet.
- **Build-side class** — [meta-yocache/classes/yocache.bbclass](meta-yocache/classes/yocache.bbclass),
  a bitbake layer enabled with `INHERIT += "yocache"`. It subscribes to a curated
  set of bitbake events and **POSTs one JSON payload per event the instant it
  fires** (no aggregation yet) to `/api/build-report`, also appending each payload
  to a JSONL log (`YOCACHE_LOG`).

The Go `buildReport` struct mirrors the bbclass JSON payload by hand — if you
change the shape on one side, change it on the other. The bbclass header comment
documents exactly which events carry cache signal and why others are excluded;
preserve that rationale when editing.

### Planned architecture (not yet built)

Storage split: **filesystem** for blobs · **SQLite (WAL)** for
operational/inventory/peer/conflict state · **DuckDB** for per-invocation,
per-recipe analytics. Discovery via mDNS/DNS-SD (`_yocache._tcp`). The headline
feature is **HTTP `PUT` upload** — bitbake has no built-in sstate/DL upload path,
which is the gap this project exists to bridge. Build order: single-node
sstate+DL → mDNS → peer gossip + pull → hash-equiv → full federation.

## Development

There is **no local Go toolchain**; everything runs in the container defined in
[.devcontainer/Dockerfile](.devcontainer/Dockerfile) (Go 1.26). The container
**must run as the host uid/gid** so bind-mounted files aren't root-owned and
git's `.git` ownership check passes (which keeps `go build` VCS stamping working).
Use a rootless engine, or pass `--user` explicitly with rootful Docker. See the
README "Development" section for the VS Code, devcontainer-CLI, and plain
docker/podman invocations.

```sh
# Build the toolchain image once (context = .devcontainer)
docker build -t yocache-dev .devcontainer            # or: podman build ...

# Compile / run (rootless Podman shown; for rootful Docker swap to --user "$(id -u):$(id -g)")
podman run --rm -it --userns=keep-id -v "$PWD":/workspace -w /workspace yocache-dev go build ./...
podman run --rm -it --userns=keep-id -p 6768:6768 -v "$PWD":/workspace -w /workspace yocache-dev \
  go run ./cmd/yocache --addr :6768
```

Module/build caches persist under `./.cache` (git-ignored), so rebuilds are fast.
There are no Go tests yet; `go test ./...` is the convention once they land.

## Exercising the cache against a real build

[testdata/yocto/](testdata/yocto/) is a reproducible kas-based Yocto build
(Wrynose / 6.0, `qemux86-64`, `core-image-minimal`) used to drive real traffic at
the server. **Run the Yocto build via `kas-container` on the host, not inside the
Go devcontainer** — yocache runs separately and they talk only over HTTP. See
[testdata/yocto/README.md](testdata/yocto/README.md) for the full manual
flow (fetch-only first look, then full cold-sstate build). For a fast turnaround,
[notes/cheap-build-targets.md](notes/cheap-build-targets.md) lists small
`*-native` targets (e.g. `quilt-native`) that fire useful event batches in
seconds instead of building a whole image.

### Running ad-hoc commands inside the build environment

To exercise yocache against bitbake's *own* client code (e.g. the hash-equiv
protocol — see below) without a full build, run a one-off command in the
kas-container, which already has bitbake's libs on `PYTHONPATH` and `websockets`
installed. From `testdata/yocto/example-project`:

```sh
KAS_IMAGE_VERSION=5.2 KAS_CONTAINER_ENGINE=podman \
  ../bin/kas-container-5.2 --runtime-args "--network=host" \
  shell yocache.yml -c "python3 /work/_some_script.py"
```

`--network=host` lets the command reach a yocache server bound on the host at
`localhost:6768`. Path mapping inside the container: the kasfile's git repo (the
yocache repo root) is `/repo`, and `KAS_WORK_DIR` (i.e. `example-project`, the
dir you run from) is `/work` — so a script written to
`testdata/yocto/example-project/foo.py` is `/work/foo.py` inside. `import
hashserv` / `import bb` resolve from the sourced bitbake env (fallback:
`/work/bitbake/lib`).

### Hash-equivalence (`BB_HASHSERVE` over WebSocket)

yocache speaks bitbake's hash-equivalence protocol ("OEHASHEQUIV" over
`bb.asyncrpc`) natively over **WebSocket**, on the same port 6768, at
`/hashequiv` (see [cmd/yocache/hashequiv.go](cmd/yocache/hashequiv.go)). Point a
build at it from `local.conf`/`site.conf` (not the bbclass — cooker reads
`BB_HASHSERVE`, a per-recipe class can't set it):

```
BB_HASHSERVE = "ws://localhost:6768/hashequiv"
```

This is the thin slice: an **in-memory** store with first-write-wins unihashes
and **no cross-output equivalence dedup** yet (a reported `outhash` never unifies
two different taskhashes). It already shares unihashes for *identical* taskhashes
across machines; output-based equivalence and SQLite persistence are follow-ups.

Analyze captured telemetry with the DuckDB scripts over the JSONL event log:

```sh
scripts/summarize-builds.sh [path/to/yocache-events.jsonl]   # one row per build
scripts/summarize-events.sh [path/to/yocache-events.jsonl]   # counts per event type
```

`notes/` holds source-verified references (bitbake event catalogue, git mirror
tarball shapes) backing the design — consult them before guessing at bitbake
behaviour.

## Conventions

- **Telemetry must never break a build.** In the bbclass, every network/parse
  failure is caught and downgraded to `bb.warn`; keep it that way.
- Default port is **6768** throughout (binary, bbclass, mDNS service name).
- The bbclass is deliberately scaffolding: synchronous per-event POSTs mean
  hundreds-to-thousands of requests per build. Batching is required before this
  is enabled on large builds — don't treat the current shape as the target.
