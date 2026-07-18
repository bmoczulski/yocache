---
title: Running with Docker
description: Run the YoCache server as a container — Docker, Podman, and Docker Compose examples.
---

Every release publishes a multi-arch (`linux/amd64`, `linux/arm64`) image to
two registries, so you don't need a Go toolchain or the release tarball just
to run the server:

| Registry | Image |
| --- | --- |
| GHCR | `ghcr.io/bmoczulski/yocache` |
| Docker Hub | `docker.io/moczulski/yocache` |

Both point at the same image; use whichever your infrastructure already
pulls from. Tags follow the release version without the `v` prefix (e.g.
`0.1.8`), plus a rolling `latest`.

The container runs as a non-root user (uid `10001`) by default, exposes port
`6768` (HTTP + hash-equivalence WebSocket) and `6767` (the legacy raw-TCP
hash-equivalence listener for pre-Scarthgap bitbake), and expects all
persistent state under a single volume at `/var/lib/yocache` — matching the
server's own `--data-dir` layout (see
[Server configuration](../server-configuration/)).

## Docker

```sh
mkdir -p ./yocache-data
docker run -d --name yocache \
  -p 6768:6768 -p 6767:6767 \
  -v "$PWD/yocache-data":/var/lib/yocache \
  ghcr.io/bmoczulski/yocache:latest
```

The container starts as root just long enough to make sure
`./yocache-data` is owned by uid `10001`, then drops privileges before
running the server — nothing to `chown` yourself first, even on a fresh
directory.

If you'd rather the container run as your own user throughout (so files on
disk are owned by you, not uid `10001`), pass `--user`:

```sh
docker run -d --name yocache \
  --user "$(id -u):$(id -g)" \
  -p 6768:6768 -p 6767:6767 \
  -v "$PWD/yocache-data":/var/lib/yocache \
  ghcr.io/bmoczulski/yocache:latest
```

In that mode the container never has root, so it trusts that you already
own the directory — which you do, since it's yours.

## Podman

Works the same way, rootless by default:

```sh
mkdir -p ./yocache-data
podman run -d --name yocache \
  -p 6768:6768 -p 6767:6767 \
  -v "$PWD/yocache-data":/var/lib/yocache:Z \
  docker.io/moczulski/yocache:latest
```

Two Podman-specific notes:

- The `:Z` volume flag relabels the directory for SELinux (Fedora, RHEL,
  CentOS); it's a no-op — and harmless — everywhere else.
- Unlike `docker`, `podman` does **not** default a bare `moczulski/yocache`
  to Docker Hub — it needs the fully-qualified `docker.io/` prefix (or a
  configured `unqualified-search-registries` in
  `/etc/containers/registries.conf`). `ghcr.io/bmoczulski/yocache` works
  either way since it already names its registry host.

## Docker Compose

```yaml
services:
  yocache:
    image: ghcr.io/bmoczulski/yocache:latest
    restart: unless-stopped
    ports:
      - "6768:6768"
      - "6767:6767"
    volumes:
      - yocache-data:/var/lib/yocache

volumes:
  yocache-data:
```

A named volume like `yocache-data` above is created empty and owned by
whatever the image itself sets it up as — no chown dance needed at all,
same effect as the root-start self-heal path.

## Configuring the container

Don't override the command to pass flags — that replaces the image's whole
default command (`--addr`, `--hashequiv-addr`, `--data-dir`), so you'd have
to repeat all of them. Use the `YOCACHE_<FLAG>` environment variables
instead (see the full list in
[Server configuration](../server-configuration/)):

```sh
docker run -d --name yocache \
  -e YOCACHE_QUOTA=500GiB \
  -e YOCACHE_EVICT=lru \
  -p 6768:6768 -p 6767:6767 \
  -v "$PWD/yocache-data":/var/lib/yocache \
  ghcr.io/bmoczulski/yocache:latest
```
