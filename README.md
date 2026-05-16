# yocache

A smart, writable cache server for [Yocto](https://www.yoctoproject.org/) /
bitbake build farms. It eliminates redundant fetching and rebuilding across
machines, users, and project configurations by acting as a shared, federated
cache that builds can both read from **and** write back to.

> Status: **early scaffold**. Only a health endpoint exists today.

## The problem

Yocto build farms re-fetch sources and re-run tasks that a neighbouring node,
another user, or another configuration on the same machine has already done.
The community workaround (NFS or a read-only nginx sstate mirror + manual
`rsync`) has no discovery, no write-back, no observability, and no federation.

## Design at a glance

Three independent cache concerns:

- **DL mirror** — source tarballs / patches / git snapshots (checksum-verified
  against bitbake's expected sums).
- **sstate cache** — task output artifacts; the primary win (content-addressed
  by input hash, correct by construction).
- **hash-equiv** — deferred (later phase; likely a single authoritative
  instance, possibly reusing bitbake-hashserv).

Key decisions:

- **Go**, single static binary, delivered as a distro package or docker image.
  Compiling on the target build server is an explicit non-goal.
- **mDNS / DNS-SD** discovery (`_yocache._tcp`) with a seed-list fallback for
  cross-subnet topologies.
- **Federation** via gossiped metadata + pull-on-demand for blobs; nodes stay
  useful during a network partition.
- **bitbake integration** ships a `yocache.bbclass` (`INHERIT += "yocache"`)
  that HTTP `PUT`s new artifacts and `POST`s a build report on
  `BuildCompleted` — bridging bitbake's missing upload path. Announced to
  builds via `SSTATE_MIRRORS` / `PREMIRRORS`.
- **Storage split**: filesystem for blobs · SQLite (WAL) for
  operational/inventory/conflict/peer state · DuckDB for per-invocation,
  per-recipe analytics.
- **Admin-gated auto-upgrade** with Ed25519-signed releases.

Build sequencing: single-node sstate+DL server → mDNS → peer gossip + pull →
hash-equiv → full federation.

## Development

No local Go install is needed. The whole toolchain lives in a container
defined once in `.devcontainer/Dockerfile` (Go 1.26) and consumed three
equivalent ways.

**Requirement:** the container must run with your host uid/gid, so nothing it
writes is root-owned and git's ownership check on `.git` passes (keeping
`go build` VCS stamping working). Use a rootless engine, or pass `--user`
explicitly with rootful Docker — see below.

### 1. VS Code

Open the folder → **Reopen in Container**. Terminal, build, debug and `gopls`
all run inside. Use a rootless engine so the container user maps to you
automatically; for Podman set `"dev.containers.dockerPath": "podman"` in VS
Code settings.

### 2. devcontainer CLI (IDE-agnostic)

```sh
npx @devcontainers/cli up   --workspace-folder .
npx @devcontainers/cli exec --workspace-folder . go build ./...
```

### 3. Plain docker / podman (quick fixes, CI, new contributors)

```sh
# Build the toolchain image once (context = .devcontainer)
docker build -t yocache-dev .devcontainer          # or: podman build ...

# Compile — Docker (rootful): pass your uid/gid explicitly
docker run --rm -it --user "$(id -u):$(id -g)" \
  -v "$PWD":/workspace -w /workspace yocache-dev go build ./...

# Compile — rootless Podman: uid maps automatically
podman run --rm -it --userns=keep-id \
  -v "$PWD":/workspace -w /workspace yocache-dev go build ./...

# Run the daemon — Docker (rootful)
docker run --rm -it --user "$(id -u):$(id -g)" -p 6768:6768 \
  -v "$PWD":/workspace -w /workspace yocache-dev \
  go run ./cmd/yocache --addr :6768

# Run the daemon — rootless Podman
podman run --rm -it --userns=keep-id -p 6768:6768 \
  -v "$PWD":/workspace -w /workspace yocache-dev \
  go run ./cmd/yocache --addr :6768
```

Then `curl localhost:6768/healthz`. Module and build caches persist in
`./.cache` (git-ignored), so repeated builds are fast.

## Repository layout

```
.devcontainer/   containerised Go toolchain (dev / CLI / CI)
cmd/yocache/     daemon entrypoint
```

More packages are added as functionality lands.
