---
title: FAQ
description: Frequently asked questions about YoCache.
---

## Which Yocto releases does it work with?

The `meta-yocache` layer declares compatibility with every release from
**dunfell** through the current **wrynose** (dunfell, gatesgarth, hardknott,
honister, kirkstone, langdale, mickledore, nanbield, scarthgap, styhead,
walnascar, wrynose). Dunfell, kirkstone, and wrynose are build-tested
end-to-end; the releases in between share the same code paths as one of
those three, so they're covered too. The optional hash-equivalence
integration works on every supported release: bitbake's WebSocket transport
requires **scarthgap or newer** (`ws://yourcache.local:6768/hashequiv`), and
older releases — whose hash-equivalence client has no `ws://` support — use
the server's raw-TCP listener instead (`yourcache.local:6767`, on by
default). See [Client configuration](/client-configuration/) for the
`BB_HASHSERVE` values.

## Can a cache problem break my build?

No — that's a design rule, not an accident. A cache miss means bitbake falls
back to upstream sources and local building, exactly as without YoCache. And
if the server is unreachable or an upload fails, the build carries on and you
get at most a warning. YoCache only ever makes builds faster, never redder.

## What happens when the disk fills up?

Give the server a `--quota` (say, `500GiB`) and an eviction policy
(`--evict lru`), and it frees space on demand by dropping the artifacts
nobody has touched for the longest time. The cache stays within budget
without anyone cleaning it manually.

## One recipe produces broken sstate — do I have to flush the whole cache?

No. Start the server with `--block-recipe <name>` and the cache refuses to
store or serve that recipe's sstate while everything else keeps flowing.
Repeat the flag for multiple recipes.

## Is it safe to point many machines at one server?

Yes — that's the intended setup. Uploads are written atomically, so a reader
never sees a half-uploaded artifact, and concurrent uploads of the same
artifact resolve cleanly.

## Do I need a database or other services?

No. The server is one static binary; its operational state lives in a single
SQLite file it manages itself. There is nothing else to install, and moving
the cache to another machine is a matter of copying a directory.
