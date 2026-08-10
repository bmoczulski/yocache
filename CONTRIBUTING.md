# Contributing to YoCache

Thanks for taking an interest. This page covers building YoCache from
source, running the tests, exercising the server against a real Yocto
build, and the conventions a change is expected to follow.

Looking to *use* YoCache rather than hack on it? Start at
[yocache.dev](https://yocache.dev) instead.

## Repository layout

```
cmd/yocache/        daemon (Go)
meta-yocache/       bitbake layer (bbclass + uploader)
site/               documentation site (Astro/Starlight → yocache.dev)
docs/               contributor reference docs
testdata/yocto/     reproducible kas build for integration testing
.devcontainer/      containerised Go toolchain
```

## Building from source

There is no local Go install needed — the toolchain lives in a container
defined in [.devcontainer/Dockerfile](.devcontainer/Dockerfile) (Go 1.26).

**Requirement:** the container must run with your host uid/gid, so nothing
it writes ends up root-owned and git's ownership check on `.git` passes
(which is what keeps `go build`'s VCS stamping working). Use a rootless
engine, or pass `--user` explicitly with rootful Docker.

The helper scripts wrap all of this (rootless Podman by default):

```sh
./build.sh   # go build + go vet + go test -race, all ./cmd/...
./shell.sh   # interactive shell in the toolchain container
./serve.sh   # run the locally-built ./yocache binary
```

Or drive the container yourself:

```sh
# Build the toolchain image once (context = .devcontainer)
docker build -t yocache-dev .devcontainer          # or: podman build ...

# Compile — Docker (rootful): pass your uid/gid explicitly
docker run --rm -it --user "$(id -u):$(id -g)" \
  -v "$PWD":/workspace -w /workspace yocache-dev go build ./...

# Compile — rootless Podman: uid maps automatically
podman run --rm -it --userns=keep-id \
  -v "$PWD":/workspace -w /workspace yocache-dev go build ./...

# Run the daemon
podman run --rm -it --userns=keep-id -p 6768:6768 \
  -v "$PWD":/workspace -w /workspace yocache-dev \
  go run ./cmd/yocache --addr :6768
```

Module and build caches persist in `./.cache` (git-ignored), so repeated
builds are fast.

## Tests

Tests live alongside the code (`*_test.go` in `cmd/yocache/`). `./build.sh`
runs the whole suite — the same one CI runs. For a single test:

```sh
go test -race -run TestName ./cmd/yocache
```

## Editor setup

### VS Code

Open the folder → **Reopen in Container**. Terminal, build, debug, and
`gopls` all run inside. Use a rootless engine so the container user maps to
you automatically; for Podman set `"dev.containers.dockerPath": "podman"` in
your VS Code settings.

### devcontainer CLI (IDE-agnostic)

```sh
npx @devcontainers/cli up   --workspace-folder .
npx @devcontainers/cli exec --workspace-folder . go build ./...
```

## Exercising the cache against a real build

[testdata/yocto/](testdata/yocto/) is a reproducible kas-based Yocto build
used to drive real traffic at the server. Run it via `kas-container` on the
host, **not** inside the Go devcontainer — YoCache runs separately and the
two talk only over HTTP. See
[testdata/yocto/README.md](testdata/yocto/README.md) for the full flow, and
[docs/cheap-build-targets.md](docs/cheap-build-targets.md) for targets that
produce useful traffic in seconds rather than rebuilding a whole image —
both for a cold tree and for a warm one you don't want to disturb.

## Conventions

- **Commits** follow [Conventional Commits](https://www.conventionalcommits.org/).
  Keep them small and surgical. No emojis.
- **Telemetry must never break a build.** In the bbclass, every network or
  parse failure is caught and downgraded to `bb.warn`; keep it that way.
  Likewise on the server, ledger and inventory failures are logged, never
  fatal to a request.
- **Every user-facing change needs a `CHANGELOG.md` entry** under
  `## Unreleased`, in the same commit or PR as the change. This is what the
  release workflow turns into GitHub Release notes — a push to `main`
  without one fails the release job outright.
- **Every user-facing change also needs a `site/` update**, in the same
  commit or PR: new or changed flags in
  [server-configuration.md](site/src/content/docs/server-configuration.md),
  new or changed bitbake variables in
  [client-configuration.md](site/src/content/docs/client-configuration.md),
  and anything that invalidates an existing answer in
  [faq.md](site/src/content/docs/faq.md) or the setup snippet in
  [getting-started.md](site/src/content/docs/getting-started.md). Nothing
  enforces this automatically — it's on the author to remember.
- **Don't guess at bitbake behaviour.** It has enough corners that a
  plausible-sounding assumption is usually wrong. Check it against the
  bitbake source, or against a real build under
  [testdata/yocto/](testdata/yocto/), before building on it.

## Releases

Every push to `main` that changes shippable content is auto-released:
[.github/workflows/release.yml](.github/workflows/release.yml) runs the test
suite, cuts `CHANGELOG.md`'s `Unreleased` section into a new version, tags
it, and publishes a GitHub Release with binaries and container images.
[VERSION](VERSION) holds `MAJOR.MINOR` — only the patch auto-increments, so
bump major or minor yourself by editing that file.
