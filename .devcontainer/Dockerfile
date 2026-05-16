# Single source of truth for the yocache dev/build toolchain.
#
# Consumed three equivalent ways (see README "Development"):
#   - VS Code "Reopen in Container"        (daily development)
#   - devcontainer CLI: `devcontainer up`  (IDE-agnostic / scripted)
#   - plain `docker`/`podman run`          (quick fixes, CI, new contributors)
#
# DEV REQUIREMENT: the container MUST run with the host user's uid/gid
#   - Docker:          --user $(id -u):$(id -g)
#   - rootless Podman: --userns=keep-id
# This is intentional: matching uids means (a) no root-owned files to
# `sudo chown` after the container exits, and (b) git's ownership check on
# the bind-mounted .git passes, so `go build`'s VCS stamping works with no
# safe.directory workaround. We deliberately do NOT bake a user into the
# image; ownership is solved by the runtime uid match, not a hardcoded uid.
#
# The image ref is fully qualified (docker.io/library/...): Docker defaults
# unqualified names to Docker Hub, but Podman does not unless the host has
# unqualified-search-registries configured, which is not the default on
# Debian/Ubuntu. Qualifying it keeps the "one Dockerfile, every engine"
# guarantee true with zero host setup.
FROM docker.io/library/golang:1.26-bookworm

# Keep every writable path inside the bind-mounted, git-ignored ./.cache so an
# arbitrary host uid (which has no /etc/passwd entry in the image) still has a
# usable HOME and persistent caches across container rebuilds.
ENV HOME=/workspace/.cache/home \
    GOPATH=/workspace/.cache/go \
    GOMODCACHE=/workspace/.cache/go/pkg/mod \
    GOCACHE=/workspace/.cache/go/build
