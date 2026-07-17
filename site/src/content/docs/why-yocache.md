---
title: Why YoCache
description: The problem with sharing Yocto caches by hand, and how YoCache solves it.
---

## The problem

Yocto builds are famously heavy — but most of that work has already been done
by somebody on your team. The sstate cache and the downloads directory exist
precisely so results can be reused. Sharing them across a team, though, is
where things get awkward:

- **bitbake can fetch from a shared mirror, but it can't publish to one.**
  Mirrors are read-only from the build's point of view, so someone has to get
  the artifacts onto the mirror by other means.
- **So teams hand-roll it**: rsync jobs from a "blessed" build machine, cron
  scripts, NFS mounts, wiki pages explaining the ritual. These mirrors go
  stale, break silently, and always end up being one person's unofficial job.
- **Stale mirrors quietly waste everyone's time.** Every artifact the mirror
  doesn't have is re-downloaded and rebuilt on every machine that needs it —
  the cost is invisible because the build still succeeds, just slower.

## What YoCache does about it

YoCache is a **writable** cache server. The `meta-yocache` layer hooks into
your builds so that whenever a machine downloads a source archive or produces
an sstate object the cache doesn't have yet, it uploads it — automatically,
in the background, while the build runs. The next machine to need that
artifact gets it from the cache.

The result is a mirror that maintains itself:

- **No sync machinery.** There is no rsync, no cron, no blessed builder. The
  builds themselves keep the cache current.
- **First build pays, everyone else benefits.** A colleague's overnight build
  or a CI run warms the cache for the whole team the moment it finishes.
- **Never in your way.** A cache miss just means bitbake falls back to
  upstream, like it always did. YoCache is designed so that no cache or
  network failure can ever break a build.

## More than a file server

Because YoCache understands what it's storing, it does a few things a plain
HTTP mirror never could:

- **Hash equivalence, included.** The same binary also serves bitbake's
  hash-equivalence protocol with persistent state, cutting rebuilds that
  differ in inputs but not in output — without running and operating a
  separate service.
- **Self-managing storage.** Set a size quota and YoCache evicts the
  least-recently-used artifacts to stay under it. You never babysit a full
  disk.
- **Visibility.** Cache hits, misses, uploads, and evictions are recorded in
  append-only logs, and a stats endpoint summarizes what's in the cache — so
  you can actually see what your cache is doing for you.
- **An escape hatch for bad artifacts.** If one recipe is known to produce
  broken sstate, block it by name on the server and the cache ignores it
  while everything else keeps working.
