---
title: Getting started
description: Deploy the YoCache server and enable it in your Yocto build in a few minutes.
---

YoCache has two halves that you set up once:

- the **server** — a single static binary that stores and serves your shared
  sstate cache and source downloads;
- the **`meta-yocache` layer** — enables your bitbake builds to use the cache
  and upload new artifacts to it automatically.

## Deploy the server

Download a pre-built binary for your platform from the
[Releases](https://github.com/bmoczulski/yocache/releases) page and run it:

```sh
./yocache
```

By default it listens on port `6768` and stores artifacts under `var/` in the
current directory — see [Server configuration](/server-configuration/) for
the flags that change this.

Quick check once it's running:

```sh
curl http://yourcache.local:6768/healthz
```

## Enable it in your build

### With kas

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
    YOCACHE_URL = "http://yourcache.local:6768"

    # OPTIONAL: use YoCache as the hash-equivalence server — auto-picks the
    # ws:// endpoint on Yocto >= Scarthgap, or the raw-TCP listener on older
    # releases (whose bitbake has no ws:// client), same line either way
    # BB_HASHSERVE = "${@'ws://yourcache.local:6768/hashequiv' if hasattr(__import__('hashserv'), 'ADDR_TYPE_WS') else 'yourcache.local:6767'}"

    # "toaster" is necessary for YoCache to harvest MissedSstate events
    INHERIT += "toaster"

    # Toaster server suggests to enable build history with commits
    INHERIT += "buildhistory"
    BUILDHISTORY_COMMIT = "1"

    # The juice!
    INHERIT += "yocache"
```

### Without kas (manual `bblayers.conf`)

```sh
git clone https://github.com/bmoczulski/yocache.git
```

In `bblayers.conf`:

```
BBLAYERS += "/path/to/yocache/meta-yocache"
```

Then add the same configuration lines as in the kas snippet above to your
`local.conf` or `site.conf`.

## Build

That's it — build as usual. The first build populates the cache as it goes;
every subsequent build, on any machine pointed at the same server, fetches
what's already there and uploads whatever it had to build or download fresh.

If the cache doesn't have something, nothing changes for you: bitbake simply
falls back to the upstream sources and builds locally, exactly as it would
without YoCache.
