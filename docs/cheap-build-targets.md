# Cheap build targets: early vs. last

Exercising [`yocache.bbclass`](../meta-yocache/classes/yocache.bbclass) doesn't
always need a full `core-image-minimal` rebuild — that would be a huge waste of
time. Which shortcut you want depends on where your build tree already is.

The rankings come from the real dependency graph — `bitbake -g
core-image-minimal`, parse-only, in the kas container (Wrynose / Yocto 6.0,
`qemux86-64`, `poky`), captured 2026-05-23. The full image pulls in **238
recipes**. To refresh them, see [Regenerating the
ranking](#regenerating-the-ranking).

## Starting clean? Build an early target

Nothing cached yet and you don't want to wait. Pick something near the bottom of
the graph. **Depends on** = how many other recipes have to be built before this
one. Every recipe here is 14 tasks, so even the leaf fires a useful batch.

| Target | Depends on | Notes |
|---|---|---|
| `quilt-native` | **0** | pure leaf — depends on nothing |
| `zlib-native` / `zstd-native` / `gnu-config-native` | **1** | real upstream tarball fetch |
| `pigz-native` | 2 | |
| `m4-native` | 4 | first shallow chain (gettext-minimal, gnu-config, texinfo-dummy) |
| `autoconf-native` | 5 | |
| `automake-native` | 6 | |
| `libtool-native` | 7 | |

Other recipes needing only `quilt-native`: `lzlib-native`, `ldconfig-native`,
`makedevs-native`, `perlcross-native`, `texinfo-dummy-native`, `tzcode-native`,
`unzip-native`, `update-rc.d-native`, `gettext-minimal-native`.

## Already built `core-image-minimal`? Force a last target

Opposite problem: the tree is warm and you want to exercise a rebuild without
torching it. **Depended on by** = how many other recipes get rebuilt if you
force this one. Force something high and you rebuild the world.

| Recipe | Depended on by | Notes |
|---|---|---|
| `core-image-minimal` | **0** | the image — the only recipe nothing depends on |
| `packagegroup-core-boot` | 1 | pure packaging, no compile |
| `run-postinsts` | 1 | target, trivial |
| `tar-native` / `ldconfig-native` / `makedevs-native` / `dnf-native` / `createrepo-c-native` / `qemu-helper-native` | 1 | one consumer rebuilds |
| `procps` | 1 | git2 source, 1 hop from `do_rootfs` — the pick for the DL/git2 repack path |
| `v86d` | 2 | tiny target that actually **compiles** → real sstate |
| `busybox` / `tar` / `sysvinit` / `eudev` / `init-ifupdown` / `modutils-initscripts` | 2 | small target packages |

Only one recipe out of 238 rebuilds nothing else, so target packages at 2 or
below are the practical sweet spot. Stay away from the other end — the toolchain
(`glibc`, `gcc-cross-*`, `binutils-cross-*`, `gcc-runtime`, core `*-native`
libs) rebuilds hundreds.

Force one at a time and watch the server log:

```sh
./shell.sh -c "bitbake -C compile v86d"          # capital -C: invalidate do_compile, rerun it + downstream
./shell.sh -c "bitbake -C install init-ifupdown" # install-only recipes
```

## Getting a real cache miss

`cleansstate` alone does **not** exercise the upload path. It drops the local
object, and the next build restores it straight from the yocache mirror — a
fetch, not a rebuild, so nothing new is ever uploaded.

To force a genuine miss, stop the build reading sstate at all:

```
YOCACHE_SKIP_FETCH_TYPES = "sstate"
```

Now the recipe really rebuilds and the result gets uploaded. Deleting the object
from the server's store works too, but the variable is easier to undo.

The DL path needs its own nudge: with a warm `DL_DIR` nothing is refetched.
Delete a specific `cache/downloads/git2_*.tar.gz` and refetch to exercise it.

## Running a target

The layer talks to a server over HTTP, so start one first, from the repo root:

```sh
./serve.sh                 # listens on :6768
```

Then, from `testdata/yocto/example-project`:

```sh
./shell.sh -c "bitbake quilt-native"
```

`shell.sh` wraps the pinned kas-container with `yocache.yml` and
`--network=host`, which is what lets the build reach a server bound on the host.
Watch the server's log, or poll `curl localhost:6768/api/stats`, to see what
moved.

## Regenerating the ranking

```sh
cd testdata/yocto/example-project
./shell.sh -c "bitbake -g core-image-minimal"   # writes build/task-depends.dot
```

Walk `task-depends.dot`: strip `.do_<task>` to get the recipe name, build a
recipe→recipe edge set, and count each recipe's transitive dependencies — the
smallest is the cheapest early target. Reverse the edges and count the same way
to get what depends on it — the smallest is the safest to force on a warm build.
