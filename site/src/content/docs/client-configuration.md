---
title: Client configuration
description: meta-yocache variables that control how your build uses the cache.
---

Everything on the build side is a bitbake variable, set in
`local.conf`/`site.conf` (or a kas `local_conf_header`) next to the
`INHERIT += "yocache"` line — see [Getting started](/getting-started/) for
the full setup snippet. Only `YOCACHE_URL` is normally needed; the rest are
opt-outs and tuning knobs with working defaults.

| Variable | Default | What it does |
| --- | --- | --- |
| `YOCACHE_URL` | `http://localhost:6768` | Where your YoCache server lives. The layer wires bitbake's download and sstate mirrors to it and enables automatic uploads. |
| `BB_HASHSERVE` | *(unset)* | Optional: use YoCache as the hash-equivalence server. On Yocto ≥ Scarthgap, point it at `ws://yourcache.local:6768/hashequiv`; on older releases (whose bitbake has no `ws://` client) point it at the server's raw-TCP listener instead, `yourcache.local:6767`. Must be set in `local.conf`/`site.conf`. |
| `YOCACHE_SKIP_FETCH_TYPES` | *(empty)* | Artifact types **not** to fetch from the cache: `sstate`, `downloads`, or `all`. With `all` the build never reads from YoCache but still uploads — a populate-only mode. |
| `YOCACHE_SKIP_UPLOAD_TYPES` | *(empty)* | Artifact types **not** to upload: `sstate`, `downloads`, or `all`. With `all` the build only consumes the cache, never feeds it. |
| `YOCACHE_SKIP_UPLOAD` | `0` | Dry run: log what *would* be uploaded, but don't send anything. |
| `YOCACHE_BLOCK_RECIPES` | *(empty)* | Space-separated recipe names never uploaded from this build — the client-side counterpart of the server's `--block-recipe`. |
| `YOCACHE_UPLOAD_THREADS` | `4` | How many artifacts are uploaded in parallel. |
| `YOCACHE_LOG` | `${TMPDIR}/yocache-events.jsonl` | Local JSONL log of the build events the layer observes. Set empty to disable. |
| `YOCACHE_LOG_LIMIT` | `10` | Cap per event type in `YOCACHE_LOG`, keeping it skimmable; `0` = unlimited. Doesn't affect what's reported to the server. |
| `YOCACHE_SKIP_POST` | `0` | Don't send build telemetry to the server (events still go to `YOCACHE_LOG`). |

## Common setups

**A CI node that warms the cache but must not depend on it:**

```
YOCACHE_SKIP_FETCH_TYPES = "all"
```

**A developer machine that consumes the cache without contributing** (say,
on a slow uplink):

```
YOCACHE_SKIP_UPLOAD_TYPES = "all"
```

**Trying YoCache out without touching the server** — see what a build would
upload, without sending a byte:

```
YOCACHE_SKIP_UPLOAD = "1"
```

For the server-side flags, see
[Server configuration](/server-configuration/).
