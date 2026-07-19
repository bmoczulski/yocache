# meta-yocache

Build-side integration layer for yocache. `classes/yocache.bbclass` wires
bitbake's DL/sstate mirrors at a yocache server and pushes artifacts to it
automatically as they're produced.

## Use

Add this layer to `bblayers.conf` (or, with kas, a `repos:` entry that points
at the repo holding it with `layers: { meta-yocache: }`), then in
`local.conf`:

```
INHERIT += "yocache"
YOCACHE_URL = "http://yocache.local:6768"
```

## What it does

- Prepends `PREMIRRORS`/`SSTATE_MIRRORS` so downloads and sstate are fetched
  from `YOCACHE_URL` before falling back upstream.
- Uploads every sstate object (plus `.siginfo`/`.sig` sidecars) and every DL
  artifact (mirror tarballs, plain `SRC_URI` fetches) the instant it's
  produced, via a cooker-resident uploader thread — see
  `notes/sstate-upload-hook.md`.
- Prints a one-line "yocache summary" at the end of each build (objects
  reused/contributed, time saved), sourced from `GET /api/build-stats`.

Network or parse failures are downgraded to `bb.warn`/`bb.note` — none of
this can fail a build.
