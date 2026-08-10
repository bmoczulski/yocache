---

## Pointing a build at it

The container is only the server. To fill the cache and read from it, add the
`meta-yocache` layer to your build and two lines to `local.conf`:

```
YOCACHE_URL = "http://yourcache.local:6768"
INHERIT += "yocache"
```

The first build populates the cache as it goes; every build after that, on any
machine pointed at the same server, pulls from it and tops it up
automatically. If something isn't cached, bitbake falls back to upstream and
builds locally, exactly as it would without YoCache.

Full walkthrough, including the kas snippet:
https://yocache.dev/getting-started/

## More

| Page | Link |
| --- | --- |
| Getting started | https://yocache.dev/getting-started/ |
| Server configuration (flags, quotas, eviction) | https://yocache.dev/server-configuration/ |
| Client configuration (bitbake variables) | https://yocache.dev/client-configuration/ |
| FAQ | https://yocache.dev/faq/ |
| Releases & changelog | https://github.com/bmoczulski/yocache/releases |

Licensed under Apache-2.0.
