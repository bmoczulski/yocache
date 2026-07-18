---
title: Server configuration
description: Command-line flags for the YoCache server.
---

The server needs no configuration file — everything is a command-line flag,
and every flag has a sensible default:

```sh
./yocache --addr :6768 --quota 500GiB --evict lru
```

| Flag | Default | What it does |
| --- | --- | --- |
| `--addr` | `:6768` | Address the HTTP server listens on. |
| `--data-dir` | `var` | Root directory for all persistent state: the operational database (`yocache.db`), the blob stores (`downloads/`, `sstate/`), and the audit logs (`yocache.ledger.jsonl`, `yocache.access.jsonl`). |
| `--quota` | `0` (unlimited) | Total storage cap for all cached artifacts, e.g. `500GiB`. When full, eviction (below) frees space on demand. |
| `--evict` | *(none)* | Eviction policy used when the quota is reached: `lru` removes the least-recently-used artifacts first, `lru-sstate` restricts that to sstate objects. Repeat the flag to chain policies in order. |
| `--block-recipe` | *(none)* | Recipe name whose artifacts the cache should refuse to store or serve — an escape hatch for a recipe known to produce broken sstate. Repeat to block more. Never affects source downloads. |
| `--build-stats-ttl` | `720h` (30 days) | How long to retain per-build download statistics. |

Both logs are plain JSONL — one JSON object per line — so they're easy to
inspect with `jq` or load into any analytics tool.

## Health and stats endpoints

`GET /healthz` answers as soon as the server is up, and `GET /api/stats`
returns a JSON summary of what's in the cache — file counts and sizes for
downloads and sstate. Handy for dashboards and for confirming the cache is
actually filling up.

For the build-side knobs, see
[Client configuration](/client-configuration/).
