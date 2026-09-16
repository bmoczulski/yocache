---
title: Client configuration
description: BitBake variables in meta-yocache that control how your Yocto build uploads to and fetches from the shared sstate and downloads cache.
---

Everything on the build side is a bitbake variable, set in
`local.conf`/`site.conf` (or a kas `local_conf_header`) next to the
`INHERIT += "yocache"` line — see [Getting started](../getting-started/) for
the full setup snippet. Only `YOCACHE_URL` is normally needed; the rest are
opt-outs and tuning knobs with working defaults.

| Variable | Default | What it does |
| --- | --- | --- |
| `YOCACHE_URL` | `http://localhost:6768` | Where your YoCache server lives. The layer wires bitbake's download and sstate mirrors to it and enables automatic uploads. |
| `BB_HASHSERVE` | derived from `YOCACHE_URL` | The layer points bitbake's hash-equivalence at YoCache automatically: `ws://<host>/hashequiv` on Yocto ≥ Scarthgap, or the raw-TCP listener (`<host>:6767`) on older releases whose bitbake has no `ws://` client. Set it yourself in `local.conf`/`site.conf` to override (e.g. to use a different hash-equivalence server than your YoCache server). |
| `YOCACHE_SKIP_FETCH_TYPES` | *(empty)* | Artifact types **not** to fetch from the cache: `sstate`, `downloads`, or `all`. With `all` the build never reads from YoCache but still uploads — a populate-only mode. |
| `YOCACHE_SKIP_UPLOAD_TYPES` | *(empty)* | Artifact types **not** to upload: `sstate`, `downloads`, or `all`. With `all` the build only consumes the cache, never feeds it. |
| `YOCACHE_BLOCK_RECIPES` | *(empty)* | Space-separated recipe names never uploaded from this build — the client-side counterpart of the server's `--block-recipe`. Propagates downstream: a recipe that transitively `DEPENDS` on a blocked recipe is skipped too (logged as `depends on blocked recipe(s): ...`), since its own sstate can inherit a blocked recipe's non-determinism even when its taskhash doesn't show it. |
| `YOCACHE_UPLOAD_THREADS` | `4` | How many artifacts are uploaded in parallel. |
| `YOCACHE_UPLOAD_LOG_LEVEL` | `quiet` | `quiet` only logs upload failures/conflicts; `verbose` also logs a NOTE for every successful PUT and every skip (server already has it) — one line per artifact, handy for devel but noisy on routine builds. |

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

For the server-side flags, see
[Server configuration](../server-configuration/).
