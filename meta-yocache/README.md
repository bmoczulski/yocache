# meta-yocache

Build-side integration layer for yocache. `classes/yocache.bbclass` reports
build + sstate telemetry to a yocache server, POSTing each subscribed event
the instant it fires. Scaffolding stage — no artifact upload yet.

## Use

Add this layer to `bblayers.conf` (or, with kas, a `repos:` entry that points
at the repo holding it with `layers: { meta-yocache: }`), then in
`local.conf`:

```
INHERIT += "yocache"
YOCACHE_URL = "http://yocache.local:6768"
```

## What it sends

On `BuildCompleted`, `POST ${YOCACHE_URL}/api/build-report` with JSON:

| field         | source                                  |
|---------------|-----------------------------------------|
| `build_name`  | `BUILDNAME`                             |
| `machine`     | `MACHINE`                               |
| `distro`      | `DISTRO`                                |
| `hostname`    | `os.uname()[1]`                         |
| `user`        | `USER`                                  |
| `started_at`  | epoch secs, from `BuildStarted`         |
| `finished_at` | epoch secs                              |
| `sstate`      | `{missed:[...], found:[...]}`, see note |

**Note:** the `sstate` field is only present when bitbake fires the
`MissedSstate` MetadataEvent, which it currently does only if `"toaster"`
is in `INHERIT` (see `sstate.bbclass`). The report is sent regardless.

Network or parse failures are downgraded to `bb.warn` — telemetry never
fails a build.
