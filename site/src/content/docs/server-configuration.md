---
title: Server configuration
description: Command-line flags for the YoCache sstate and downloads cache server — storage quota, eviction policy, hash-equivalence ports, and more.
---

The server needs no configuration file — everything is a command-line flag,
and every flag has a sensible default:

```sh
./yocache --addr :6768 --quota 500GiB --evict lru
```

Every flag can also be set via a `YOCACHE_<FLAG>` environment variable
instead — handy for Docker and systemd, where env vars are the natural way
to configure a container or unit. A CLI flag always wins over its env var,
which wins over the compiled-in default.

<div class="no-wrap-col1">

| Flag | Env var | Default | What it does |
| --- | --- | --- | --- |
| `--addr` | `YOCACHE_ADDR` | `:6768` | Address the HTTP server listens on. |
| `--hashequiv-addr` | `YOCACHE_HASHEQUIV_ADDR` | `:6767` | Address a second, dedicated raw-TCP hash-equivalence listener binds to — for pre-Scarthgap bitbake (Dunfell and earlier), whose hash-equivalence client has no `ws://` support. On by default; set to `""` to disable it. |
| `--data-dir` | `YOCACHE_DATA_DIR` | `var` | Root directory for all persistent state: the operational database (`yocache.db`), the blob stores (`downloads/`, `sstate/`), and the audit logs (`yocache.ledger.jsonl`, `yocache.access.jsonl`). |
| `--quota` | `YOCACHE_QUOTA` | `0` (unlimited) | Total storage cap for all cached artifacts, e.g. `500GiB`. When full, eviction (below) frees space on demand. |
| `--evict` | `YOCACHE_EVICT` | *(none)* | Eviction policy used when the quota is reached: `lru` removes the least-recently-used artifacts first, `lru-sstate` restricts that to sstate objects. Repeat the flag (or comma-separate the env var, e.g. `YOCACHE_EVICT=lru,lru-sstate`) to chain policies in order. An sstate task's archive and its `.siginfo`/`.sig` sidecars are always evicted together, and evicting one also drops its hash-equivalence entry — no separate GC pass needed, unlike bitbake's own hash-equivalence server. |
| `--block-recipe` | `YOCACHE_BLOCK_RECIPE` | *(none)* | Recipe name whose artifacts the cache should refuse to store or serve — an escape hatch for a recipe known to produce broken sstate. Repeat the flag (or comma-separate the env var) to block more. Never affects source downloads. |
| `--trusted-proxy` | `YOCACHE_TRUSTED_PROXY` | *(none)* | Address or CIDR of a reverse proxy in front of YoCache, e.g. `10.0.0.5` or `10.0.0.0/24`. For requests from it, the client address in YoCache's log lines is taken from its `X-Forwarded-For` header rather than being the proxy's own address. Repeat the flag (or comma-separate the env var) for more. Only list your own proxies: the header is otherwise ignored, because any client can send it. See [Behind a reverse proxy](../reverse-proxy/#client-addresses-in-logs). |
| `--build-stats-ttl` | `YOCACHE_BUILD_STATS_TTL` | `720h` (30 days) | How long to retain per-build download statistics. |

</div>

Both logs are plain JSONL — one JSON object per line — so they're easy to
inspect with `jq` or load into any analytics tool.

## Health and stats endpoints

`GET /healthz` answers as soon as the server is up, and `GET /api/stats`
returns a JSON summary of what's in the cache — file counts and sizes for
downloads and sstate. Handy for scripting and for confirming the cache is
actually filling up.

### Hash-equivalence request statistics

The hash-equivalence protocol supports BitBake's `get-stats` command on both
the `/hashequiv` WebSocket endpoint and the raw-TCP listener. Sending
`{"get-stats": null}` after the protocol handshake returns a `requests` object
with `num`, `total_time`, `max_time`, `average`, and `stdev` fields. Timing
values are in seconds; `stdev` is the sample standard deviation (zero with
fewer than two samples).

These statistics aggregate completed hash-equivalence requests across all
connections and both transports, including individual stream messages and
their `END` markers. Idle time waiting for requests is excluded, and so is
time spent sending a command's reply (stream lines include it, as in BitBake).
The response does not include the current `get-stats` request, which is
counted once it finishes. Statistics are held in memory and start from zero on
server restart; they are separate from the persistent cache inventory at
`/api/stats`. The `reset-stats` command is not supported.

## Web dashboard

Point a browser at the server (e.g. `http://yourcache.local:6768/`) to
see a live dashboard of the same data, plus a pie chart of blob-store
byte share. It's a Svelte SPA compiled into the binary — no separate web
host, no configuration flag, no way to disable it in this release. Root
`/` returns a 301 to `/ui/` where the dashboard lives.

The dashboard uses only relative URLs, so it also works behind a reverse
proxy that strips a path prefix — e.g. several instances at
`https://proxy/cache1/`, `https://proxy/cache2/` via Traefik's `stripPrefix`.
Link to the prefix with its trailing slash (`/cache1/`): from a bare
`/cache1` the browser resolves the relative redirect to `/ui/`, outside the
prefix. See [Behind a reverse proxy](../reverse-proxy/) for a complete proxy
setup, HTTPS included.

For the build-side knobs, see
[Client configuration](../client-configuration/).
