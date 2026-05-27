# test/yocto — kas harness for observing real cache traffic

A reproducible Yocto build (Wrynose / 6.0, `qemux86-64`, console-only
`core-image-minimal`) used to drive traffic against the yocache server and,
later, as the fixture for automated integration tests.

```
example-project/base.yml      pristine minimal build, no cache wiring
example-project/yocache.yml   base + SSTATE_MIRRORS / PREMIRRORS -> yocache
bin/kas-container-5.2         pinned kas-container script
example-project/build|cache   build output + persistent DL/sstate (git-ignored)
```

## Wrynose note (why this isn't a poky clone)

Yocto 6.0 dropped the monolithic `poky` repo. 6.0 only exists as three
separate repos, pinned here exactly as `bitbake-setup`'s registry does
(`bitbake/default-registry/configurations/poky-wrynose.conf.json`):

| repo | branch |
|---|---|
| `git.openembedded.org/bitbake` | `2.18` |
| `git.openembedded.org/openembedded-core` | `wrynose` |
| `git.yoctoproject.org/meta-yocto` | `wrynose` |

`example-project/poky/` (a manual master clone) is **not** used by these
configs and is git-ignored.

## Prerequisites

- Docker or Podman on the host. `bin/kas-container-5.2` is already fetched
  (pinned). `export KAS_IMAGE_VERSION=5.2 KAS_CONTAINER_ENGINE=podman`.
- `--network=host` lets the kas build container reach the yocache server on the
  host at `localhost:6768`. Without host networking, bind yocache to `0.0.0.0`
  and use the host LAN IP in `yocache.yml`.

> Run the Yocto build via `kas-container` on the host, **not** inside the Go
> devcontainer. yocache runs separately; they talk only over HTTP :6768.

## Manual test-flow

1. **Start the void yocache** (from repo root) and tee its logs:
   ```sh
   podman run --rm --name yocache-void --userns=keep-id -p 6768:6768 \
     -v "$PWD":/workspace -w /workspace yocache-dev \
     go run ./cmd/yocache --addr :6768 | tee /tmp/yocache.log
   ```

2. **Fast first look — DL traffic only** (no compile; minutes, not hours):
   ```sh
   cd test/yocto/example-project
   ../bin/kas-container-5.2 --runtime-args "--network=host" \
     shell yocache.yml -c "bitbake core-image-minimal --runall=fetch"
   ```
   `/tmp/yocache.log` fills with `kind=downloads ... status=404`; bitbake then
   fetches upstream so it still completes.

3. **Full build — dense sstate traffic.** Force a cold sstate first:
   ```sh
   rm -rf test/yocto/example-project/cache/sstate
   cd test/yocto/example-project
   ../bin/kas-container-5.2 --runtime-args "--network=host" build yocache.yml
   ```
   Optional pristine baseline for comparison: same command with `base.yml`;
   note its `Sstate summary: Wanted .. Found .. Missed ..` line + wall time.

4. **Analyze the shape** (informs the next step — transparent proxy, then a
   real store):
   ```sh
   grep 'cache request' /tmp/yocache.log | grep -o 'kind=[a-z]*'   | sort | uniq -c
   grep 'cache request' /tmp/yocache.log | grep -o 'method=[A-Z]*'  | sort | uniq -c
   ```
   Look at sstate object naming (`sstate:<pn>:...:<hash>_<task>.tgz`), download
   filenames, and HEAD-vs-GET behaviour.
