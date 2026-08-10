# YoCache — smart cache sharing for Yocto builds

Sharing Yocto sstate-cache and downloads with your development team?

Forget rsync sessions to an HTTP server. YoCache hooks into your bitbake
builds, uploads artifacts automatically, and serves them back to everyone
— one line of config away.

**[Documentation](https://yocache.dev)** ·
[Getting started](https://yocache.dev/getting-started/) ·
[Running with Docker](https://yocache.dev/docker/) ·
[FAQ](https://yocache.dev/faq/)

## What it is

- **A server** — one static binary, or a container: a shared, *writable*
  sstate + downloads mirror with hash-equivalence built in.
- **A bitbake layer** (`meta-yocache`) — wires the mirrors up and uploads
  artifacts the moment they're produced.

bitbake can read from an sstate mirror, but it has no built-in way to *push*
to one. That gap is what YoCache exists to close.

## Quick start

### 1. Run the server

With Docker:

```sh
docker run -d --name yocache -p 6768:6768 -p 6767:6767 \
  -v "$PWD/yocache-data":/var/lib/yocache \
  ghcr.io/bmoczulski/yocache:latest
```

Or grab a binary from the
[Releases](https://github.com/bmoczulski/yocache/releases) page and run it:

```sh
./yocache
```

Either way, check it's up:

```sh
curl http://yourcache.local:6768/healthz
```

Or open `http://yourcache.local:6768/` in a browser — it shows a dashboard
of what the cache is holding.

### 2. Point your build at it

With kas, add YoCache to your kasfile:

```yaml
repos:
  yocache:
    url: https://github.com/bmoczulski/yocache.git
    branch: main
    layers:
      meta-yocache:

local_conf_header:
  yocache: |
    YOCACHE_URL = "http://yourcache.local:6768"

    # OPTIONAL: use YoCache as the hash-equivalence server — auto-picks the
    # ws:// endpoint on Yocto >= Scarthgap, or the raw-TCP listener on older
    # releases (whose bitbake has no ws:// client), same line either way
    # BB_HASHSERVE = "${@'ws://yourcache.local:6768/hashequiv' if hasattr(__import__('hashserv'), 'ADDR_TYPE_WS') else 'yourcache.local:6767'}"

    # The juice!
    INHERIT += "yocache"
```

Not using kas? Add `meta-yocache` to `bblayers.conf` and the same lines to
`local.conf` — see
[Getting started](https://yocache.dev/getting-started/).

### 3. Build as usual

The first build fills the cache; every build after that, on any machine
pointed at the same server, pulls from it and tops it up automatically. If
something isn't cached, bitbake just falls back to upstream and builds it
locally — exactly as it would without YoCache.

## Documentation

| Page | What it covers |
| --- | --- |
| [Getting started](https://yocache.dev/getting-started/) | Deploy the server, enable the layer |
| [Why YoCache](https://yocache.dev/why-yocache/) | What problem it solves, and how |
| [Running with Docker](https://yocache.dev/docker/) | Docker, Podman, Compose, volumes |
| [Server configuration](https://yocache.dev/server-configuration/) | Flags, environment variables, quotas, eviction |
| [Client configuration](https://yocache.dev/client-configuration/) | bitbake variables the layer reads |
| [FAQ](https://yocache.dev/faq/) | Common questions |

## Container images

Published to two registries on every release, multi-arch
(`linux/amd64`, `linux/arm64`):

- `ghcr.io/bmoczulski/yocache`
- `docker.io/moczulski/yocache`

## Building from source & contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) — the Go toolchain lives in a
container, so there's nothing to install locally.

## License

[Apache-2.0](LICENSE).
