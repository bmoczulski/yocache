# YoCache - smart cache sharing solution for Yocto builds

Sharing Yocto sstate-cache and downloads with your development team?

Forget rsync sessions to HTTP server! YoCache hooks into your bitbake builds,
uploads artifacts automatically, and serves them back to everyone
— one line of config away.

YoCache is:
- an HTTP server — single static binary — that acts as a shared, writable sstate + DL
  mirror with built-in hash-equivalence.
- a bitbake class (`meta-yocache`): wires the mirrors and handles automatic uploads.

## Getting started

### Deploy the server

Pre-built binaries will be on the
[Releases](https://github.com/bmoczulski/yocache/releases) page. Until then,
see [Building from source](#building-from-source) below.

Quick check once it's running:

```sh
curl http://yourcache.local:6768/healthz
```

### Add meta-yocache to your kas project

Add YoCache to your kasfile's `repos:` section:

```yaml
repos:
  yocache:
    url: https://github.com/bmoczulski/yocache.git
    refspec: main
    layers:
      meta-yocache:

local_conf_header:
  yocache: |
    YOCACHE_URL = "http://localhost:6768"

    # OPTIONAL: use YoCache web-socket for hash-equiv in Yocto >= Kirkstone
    # BB_HASHSERVE = "${@'ws://localhost:6768/hashequiv' if hasattr(__import__('hashserv'), 'ADDR_TYPE_WS') else 'auto'}"

    # "toaster" is necessary for YoCache to harvest MissedSstate events
    INHERIT += "toaster"

    # Toaster server suggests to enable build history with commits
    INHERIT += "buildhistory"
    BUILDHISTORY_COMMIT = "1"

    # The juice!
    INHERIT += "yocache"
```

### Without kas (manual bblayers.conf)

```sh
git clone https://github.com/bmoczulski/yocache.git
```

In `bblayers.conf`:

```
BBLAYERS += "/path/to/yocache/meta-yocache"
```

Add the same `local.conf` lines as above.

## Building from source

No local Go install needed — the toolchain lives in a container defined in
`.devcontainer/Dockerfile` (Go 1.26).

**Requirement:** the container must run with your host uid/gid so nothing it
writes is root-owned and git's ownership check on `.git` passes. Use a
rootless engine, or pass `--user` explicitly with rootful Docker.

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

Module and build caches persist in `./.cache` (git-ignored), so repeated
builds are fast.

## Development

### VS Code

Open the folder → **Reopen in Container**. Terminal, build, debug, and
`gopls` all run inside. Use a rootless engine so the container user maps to
you automatically; for Podman set `"dev.containers.dockerPath": "podman"` in
VS Code settings.

### devcontainer CLI (IDE-agnostic)

```sh
npx @devcontainers/cli up   --workspace-folder .
npx @devcontainers/cli exec --workspace-folder . go build ./...
```

To exercise the server against a real Yocto build, see
[testdata/yocto/README.md](testdata/yocto/README.md).

## Repository layout

```
cmd/yocache/        daemon (Go)
meta-yocache/       bitbake layer (bbclass + uploader)
testdata/yocto/     reproducible kas build for integration testing
.devcontainer/      containerised Go toolchain
```
