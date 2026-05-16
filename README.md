# yocache

A smart, writable cache server for [Yocto](https://www.yoctoproject.org/) /
bitbake build farms. It eliminates redundant fetching and rebuilding across
machines, users, and project configurations by acting as a shared, federated
cache that builds can both read from **and** write back to.

> Status: **early scaffold**. Only a health endpoint exists today.

## The problem

Yocto build farms re-fetch sources and re-run tasks that a neighbouring node,
another user, or another configuration on the same machine has already done.
The community workaround (NFS or a read-only nginx sstate mirror + manual
`rsync`) has no discovery, no write-back, no observability, and no federation.

## Design at a glance

Three independent cache concerns:

- **DL mirror** — source tarballs / patches / git snapshots (checksum-verified
  against bitbake's expected sums).
- **sstate cache** — task output artifacts; the primary win (content-addressed
  by input hash, correct by construction).
- **hash-equiv** — deferred (later phase; likely a single authoritative
  instance, possibly reusing bitbake-hashserv).

Key decisions:

- **Go**, single static binary, delivered as a distro package or docker image.
  Compiling on the target build server is an explicit non-goal.
- **mDNS / DNS-SD** discovery (`_yocache._tcp`) with a seed-list fallback for
  cross-subnet topologies.
- **Federation** via gossiped metadata + pull-on-demand for blobs; nodes stay
  useful during a network partition.
- **bitbake integration** ships a `yocache.bbclass` (`INHERIT += "yocache"`)
  that HTTP `PUT`s new artifacts and `POST`s a build report on
  `BuildCompleted` — bridging bitbake's missing upload path. Announced to
  builds via `SSTATE_MIRRORS` / `PREMIRRORS`.
- **Storage split**: filesystem for blobs · SQLite (WAL) for
  operational/inventory/conflict/peer state · DuckDB for per-invocation,
  per-recipe analytics.
- **Admin-gated auto-upgrade** with Ed25519-signed releases.

Build sequencing: single-node sstate+DL server → mDNS → peer gossip + pull →
hash-equiv → full federation.

## Build & run

Requires Go 1.26+ (development/CI only — not on deployment targets).

```sh
go run ./cmd/yocache --addr :6768
curl localhost:6768/healthz
```

## Repository layout

```
cmd/yocache/   daemon entrypoint
```

More packages are added as functionality lands.
