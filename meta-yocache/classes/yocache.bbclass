# yocache.bbclass — report build & sstate telemetry to a yocache server.
#
# Enable per build with:
#     INHERIT += "yocache"
#     YOCACHE_URL = "http://yocache.local:6768"
#
# Inheriting this class also wires bitbake's mirrors at the yocache server: it
# inherits own-mirrors and derives both SOURCE_MIRROR_URL (source downloads) and
# SSTATE_MIRRORS (sstate) from YOCACHE_URL (see the mirror wiring below), so no
# separate mirror lines are needed in local.conf. The one thing that can't be
# folded in is `INHERIT += "toaster"` (needed for the MissedSstate event): it
# must stay in local.conf, because sstate.bbclass gates MissedSstate on the
# global INHERIT variable, which a per-recipe class can't set.
#
# Scaffolding only: every event we subscribe to is POSTed to the server the
# instant it fires (no aggregation, no end-of-build report).
#
# Artifact upload is scaffolded (see the "Artifact upload" section below and
# notes/sstate-upload-hook.md): the SSTATEPOSTCREATEFUNCS / do_fetch[postfuncs]
# hooks notify a cooker-resident uploader thread (lib/yocache/uploader.py) over a
# unix socket, which PUTs each blob to the server. The server side is still a
# stub returning 501, so nothing is stored yet — the build-side plumbing is what
# this exercises.
#
# Subscribed events, kept deliberately minimal — yocache is a DL + sstate
# (+ hash-equiv) cache, so it only listens for events that carry cache signal:
#   - sstate HIT: sceneQueueTaskCompleted (task restored from sstate, not built).
#   - sstate MISS: sceneQueueTaskFailed (no usable sstate — the real task runs).
#     "setscene" is bitbake's restore-from-sstate pass; both fire unconditionally.
#   - sstate hit/miss (RICHER, optional): MetadataEvent MissedSstate, carrying
#     {missed:[...], found:[...]} as (fn, task, hash, sstatefile). Only fires when
#     "toaster" is in INHERIT (see sstate.bbclass) — a bonus, never the sole source.
#   - hash equivalence: taskUniHashUpdate (a unihash remapped to a known
#     equivalent — a build we got to skip).
#   - upload candidates: runQueueTaskCompleted (a task that actually ran, i.e. a
#     miss we rebuilt → an artifact worth pushing; carries the task hash). Plus
#     MetadataEvent TaskArtifacts (sstate manifest list; also toaster-gated).
#   - DL mirror correctness: bb.fetch2.MissingChecksumEvent (a fetch with no
#     recorded checksum — a reproducibility risk worth surfacing).
#   - build identity/bracketing: BuildStarted, BuildCompleted (machine/distro/
#     user + start/end, to attribute everything above to one build).
#
# Provenance: every payload also carries best-effort identity — hostname, local
# egress IP, host machine-id (if visible), and the driver's git user.name/email
# — so the server can group builds by user/machine even under a shared-uid kas
# container, where USER/uid/pid all collapse to the container's defaults. None
# is sufficient alone; combined they're compelling. Gathered out-of-band (never
# bitbake vars), so they can't affect sstate signatures, and memoised so the
# per-task event storm doesn't re-fork git / re-open sockets. See
# notes/build-identity.md.
#   - health: runQueueTaskFailed (a task that ran and failed — no artifact to
#     expect; carries exitcode).
#
# Deliberately NOT subscribed (no cache signal, or redundant with the above):
#   - ParseStarted/Completed, CacheLoadStarted/Completed — bitbake's own recipe
#     parse + dep-cache timing (that "cache" is NOT sstate); noise here.
#   - sceneQueueTaskStarted, runQueueTaskStarted — "began" twins; the
#     Completed/Failed pairs already give the outcome and the totals.
#   - runQueueTaskSkipped — a hit is already reported by sceneQueueTaskCompleted.
#   - bb.build.TaskSucceeded/TaskFailed — worker-side twins of the runQueue
#     events but without the task hash, so the runQueue ones win.
#   - NoProvider, DiskFull — build-config / build-host problems, not cache events.
#
# Of OE-Core's MetadataEvent subtypes only MissedSstate and TaskArtifacts are
# relevant; the Toaster-only ones (LayerInfo, SinglePackageInfo, ImagePkgList,
# SDKArtifactInfo, BuildStatsList) are dropped in-handler via
# YOCACHE_METADATA_TYPES, since eventmask can't distinguish MetadataEvent
# subtypes (they're all the same class).
#
# NOTE: the task-level events fire once per task — hundreds to thousands of
# synchronous POSTs per build. Fine for scaffolding/telemetry triage, but the
# real implementation must batch these before this is enabled on big builds.
#
# Telemetry must never break a build: every network/parse failure is caught
# and downgraded to a bb.warn.
#
# Every payload is also appended (one JSON object per line, JSONL) to
# YOCACHE_LOG so you can inspect exactly what was sent without scraping it
# out of the build's stdout/stderr.

# Base URL of the yocache server. The canonical place to set this is
# local.conf / site.conf; this weak default just keeps the class self-contained.
YOCACHE_URL ??= "http://localhost:6768"

# Endpoint that receives the end-of-build JSON report.
YOCACHE_REPORT_ENDPOINT ??= "${YOCACHE_URL}/api/build-report"

# Skip the HTTP POST entirely (events still go to YOCACHE_LOG). Useful for
# offline inspection or dry runs without a server. Accepts 1/0, yes/no,
# true/false.
YOCACHE_SKIP_POST ??= "0"

# Dedicated event log (JSONL, one payload per line). Set empty to disable.
YOCACHE_LOG ??= "${TMPDIR}/yocache-events.jsonl"

# Cap how many events of each type get written to YOCACHE_LOG (the log only —
# POSTs are unaffected). Keeps the log eyeballable instead of drowning in
# thousands of task events. 0 = unlimited.
YOCACHE_LOG_LIMIT ??= "10"

# All replacements defined in build_mirroruris() in bitbake/lib/bb/fetch2/__init__.py
#   TYPE       origud.type        e.g. git / https / crate / gitsm
#   HOST       origud.host        e.g. git.openembedded.org
#   PATH       origud.path        leading-slash repo/file path
#   BASENAME   path.split('/')[-1]
#   MIRRORNAME host+path flattened (':'->'.', '/'->'.', '*'->'.')
# YOCACHE_URL_REPLACEMENTS = "type=TYPE/host=HOST/mirrorname=MIRRORNAME/basename=BASENAME/path=PATH/"
YOCACHE_URL_REPLACEMENTS = "PATH"

# --- DL (downloads) mirror wiring ----------------------------------------
# bitbake has no notion of "the yocache server" for source fetches, so route
# its PREMIRRORS at one via own-mirrors, which prepends every fetch scheme
# (git/https/npm/crate/...) to SOURCE_MIRROR_URL. Inheriting it here means a
# plain `INHERIT += "yocache"` is enough to send downloads through yocache, with
# no separate own-mirrors line in local.conf.
inherit own-mirrors
SOURCE_MIRROR_URL += "${YOCACHE_URL}/downloads/${YOCACHE_URL_REPLACEMENTS}"
SSTATE_MIRRORS = "file://.* ${YOCACHE_URL}/sstate/${YOCACHE_URL_REPLACEMENTS};downloadfilename=PATH"

# --- Artifact upload (sstate + DL) ---------------------------------------
# The read side above pulls from yocache; this is the write side. bitbake has no
# push path, so the SSTATEPOSTCREATEFUNCS / do_fetch[postfuncs] hooks below
# notify a cooker-resident uploader thread (lib/yocache/uploader.py) over a unix
# socket, and it PUTs each blob to ${YOCACHE_URL}/{sstate,downloads}/<name>.
# Why a thread in the cooker and not the hook itself: a hook runs in a short-lived
# worker that os._exit()s, so it can neither block on the upload nor spawn a
# surviving thread — see notes/sstate-upload-hook.md.

# Unix socket the worker hooks send notifies to; the cooker uploader listens here.
# NOTE: AF_UNIX paths are capped at ~108 bytes — a very deep TMPDIR can overflow.
YOCACHE_UPLOAD_SOCK ??= "${TMPDIR}/yocache-upload.sock"

# Dry run: accept notifies and log what *would* be uploaded, but skip the PUT.
YOCACHE_SKIP_UPLOAD ??= "0"

# Size of the cooker uploader's PUT worker pool.
YOCACHE_UPLOAD_THREADS ??= "4"

# Make the git mirror tarballs actually exist so there's something to upload:
# without these, DL_DIR holds only the bare git2/<...> clone dir, no tarball
# (see notes/git-mirror-tarballs.md). Weak so a build can still opt out.
BB_GENERATE_MIRROR_TARBALLS ??= "1"
BB_GENERATE_SHALLOW_TARBALLS ??= "1"

# Which MetadataEvent .type values to keep. OE-Core fires several Toaster-only
# subtypes through the same bb.event.MetadataEvent class; eventmask can't filter
# by subtype, so the handler drops anything not listed here before any I/O. For
# a cache server only the sstate-related subtypes carry signal. Empty = keep all.
YOCACHE_METADATA_TYPES ??= "MissedSstate TaskArtifacts"

python yocache_eventhandler () {
    import json
    import os
    import pwd
    import socket
    import subprocess
    import time
    import urllib.parse
    import urllib.request

    d = e.data

    # One-time setup gate: complain LOUDLY (but never abort) if "toaster" is
    # missing from the global INHERIT. MissedSstate — yocache's richest sstate
    # hit/miss signal — is fired by sstate.bbclass only when "toaster" is in
    # INHERIT, and that check reads cooker.data (built from local.conf /
    # site.conf), NOT a per-recipe datastore. So this class genuinely cannot
    # enable it: neither `inherit toaster` nor `INHERIT += "toaster"` placed in a
    # class reaches cooker.data — only conf files do. We mirror sstate's exact
    # predicate (substring on the raw INHERIT string) so we complain iff sstate
    # would actually skip the event. bb.error, not bb.fatal: telemetry must never
    # break a build, so this is noisy, not fatal.
    if isinstance(e, bb.event.BuildStarted):
        if "toaster" not in (d.getVar("INHERIT") or ""):
            bb.error(
                "yocache: 'toaster' is not in INHERIT — sstate.bbclass will not "
                "fire MissedSstate, so yocache loses its richest sstate hit/miss "
                "signal (the always-on sceneQueueTask{Completed,Failed} events "
                "still report basic hits/misses). Fix: add "
                '`INHERIT += "toaster"` to your local.conf. It MUST live there, '
                "not in a class: sstate gates MissedSstate on the global INHERIT "
                "read from cooker.data, which only conf files populate — a "
                "class-level inherit/INHERIT touches just the recipe datastore. "
                "The build continues regardless."
            )
        # Start the cooker-resident artifact uploader for this build. This
        # handler runs in the cooker (BuildStarted fires there), the only place
        # a thread can outlive a task. Fail-soft: an upload must never break a
        # build.
        try:
            from yocache import uploader
            uploader.start(d)
        except Exception as exc:
            bb.warn("yocache: could not start uploader: %s" % exc)

    # Drain and stop the uploader before the build is considered finished. The
    # synchronous drain here is the barrier that guarantees queued PUTs complete
    # before teardown (critical in a kas-container, which exits at build end and
    # would otherwise kill in-flight uploads).
    if isinstance(e, bb.event.BuildCompleted):
        try:
            from yocache import uploader
            uploader.stop(d)
        except Exception as exc:
            bb.warn("yocache: could not stop uploader: %s" % exc)

    # Drop the Toaster-only MetadataEvent subtypes up front (before any
    # serialization or I/O) — eventmask let every MetadataEvent through because
    # they share one class, so the subtype filter has to live here.
    if isinstance(e, bb.event.MetadataEvent):
        _wanted = (d.getVar("YOCACHE_METADATA_TYPES") or "").split()
        if _wanted and getattr(e, "type", None) not in _wanted:
            return

    # Max object-graph depth to expand before falling back to repr. bitbake
    # events hang the whole DataSmart datastore off .data, so an unbounded
    # walk is both enormous and cyclic ("Circular reference detected").
    _MAX_DEPTH = 6

    def _jsonable(o, depth=0, seen=None):
        # "Shove whatever there is": bitbake event payloads carry bytes and
        # other non-JSON objects. Coerce instead of raising so the POST always
        # goes out — the server can sort out the stringified bits. Bounded by
        # depth and an id() cycle guard so the datastore can't blow this up.
        if o is None or isinstance(o, (bool, int, float, str)):
            return o
        if isinstance(o, (bytes, bytearray)):
            return o.decode("utf-8", "replace")

        if depth >= _MAX_DEPTH:
            return repr(o)

        seen = seen if seen is not None else set()
        if id(o) in seen:
            return repr(o)  # cycle — don't recurse back into it
        seen = seen | {id(o)}

        if isinstance(o, dict):
            return {str(k): _jsonable(v, depth + 1, seen)
                    for k, v in o.items()}
        if isinstance(o, (list, tuple, set, frozenset)):
            return [_jsonable(v, depth + 1, seen) for v in o]

        # bitbake events (and most payload objects) keep their state in
        # __dict__ — emit that so the server gets the actual fields instead
        # of a useless "<... object at 0x...>" repr.
        obj_dict = getattr(o, "__dict__", None)
        if obj_dict:
            return _jsonable(obj_dict, depth + 1, seen)
        return repr(o)

    def yocache_log(line, key):
        # Append the serialized payload to the dedicated event log so it can
        # be inspected without polluting stdout/stderr. Best-effort: a logging
        # failure must never break the build, and must not even warn loudly
        # (it would defeat the point of the quiet log).
        logpath = d.getVar("YOCACHE_LOG")
        if not logpath:
            return
        try:
            limit = int(d.getVar("YOCACHE_LOG_LIMIT") or 0)
        except ValueError:
            limit = 0
        try:
            logdir = os.path.dirname(logpath)
            if logdir:
                os.makedirs(logdir, exist_ok=True)
            if limit > 0:
                # Cross-process tally: task events fire from worker processes,
                # so an in-memory counter would reset per worker. Instead keep
                # one sidecar file per event type and append a single byte per
                # logged line — file size == lines logged so far. Single-byte
                # O_APPEND writes are atomic enough (same bar as the log
                # itself), and we never reread the multi-MB log. A rare
                # check/write race may let a type reach ~limit+N; fine for
                # eyeballing.
                safe = "".join(c if c.isalnum() else "_" for c in key)
                tally = "%s.%s.count" % (logpath, safe)
                try:
                    if os.path.getsize(tally) >= limit:
                        return
                except OSError:
                    pass  # missing/unstattable tally — treat as 0
                fd = os.open(tally,
                             os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o644)
                try:
                    os.write(fd, b"x")
                finally:
                    os.close(fd)
            # One write of one line: small lines append atomically enough for
            # eyeballing even though task events fire from worker processes.
            with open(logpath, "a", encoding="utf-8") as fh:
                fh.write(line + "\n")
        except Exception as exc:
            bb.note("yocache: could not write %s: %s" % (logpath, exc))

    def yocache_post(endpoint, body):
        if bb.utils.to_boolean(d.getVar("YOCACHE_SKIP_POST")):
            return
        req = urllib.request.Request(
            endpoint,
            data=body.encode("utf-8"),
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        try:
            with urllib.request.urlopen(req, timeout=5) as resp:
                resp.read()
            bb.note("yocache: posted %s to %s" % (payload.get("event"), endpoint))
        except Exception as exc:
            bb.warn("yocache: failed to POST %s: %s" % (endpoint, exc))

    def get_os_user():
        uid = os.getuid()
        try:
            return pwd.getpwuid(uid).pw_name
        except Exception:
            return str(uid)

    def yocache_identity():
        # Best-effort provenance, constant for the whole invocation, so compute
        # it once and stash it in module globals(): this handler fires per task
        # (thousands of times) and we must not re-fork git or re-open a socket
        # each time. If bitbake hands the handler a fresh globals() per call the
        # cache simply misses and we recompute — correct either way. All probes
        # are wrapped: provenance must never break a build (and a missing field
        # is just None, which the server treats as "unknown").
        cache = globals().setdefault("_yocache_identity_cache", {})
        if cache.get("_done"):
            return cache

        try:
            cache["hostname"] = os.uname()[1]
        except Exception:
            cache["hostname"] = None

        # Local egress IP toward the yocache server. A *connected* UDP socket
        # sends no packets but makes the kernel pick the source address it would
        # use to reach the server — i.e. the IP the server sees, pre-NAT.
        cache["ip"] = None
        try:
            u = urllib.parse.urlparse(d.getVar("YOCACHE_URL") or "")
            s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
            try:
                s.connect((u.hostname or "localhost", u.port or 6768))
                cache["ip"] = s.getsockname()[0]
            finally:
                s.close()
        except Exception:
            pass

        # Host machine-id, iff the file is visible (bare-metal build, or the
        # host's path bind-mounted into the container). Mounting/minting our own
        # is a later decision; for now we just read what's there.
        cache["machine_id"] = None
        for p in ("/etc/machine-id", "/var/lib/dbus/machine-id"):
            try:
                with open(p) as fh:
                    v = fh.read().strip()
                if v:
                    cache["machine_id"] = v
                    break
            except Exception:
                pass

        # git identity of the human driving the build — the strongest signal we
        # have, and it survives a shared-uid container. `git config --get` reads
        # the whole system/global/local hierarchy, so it resolves even outside a
        # repo; running it from the metadata tree also honours repo-local config.
        cache["git_user_name"] = None
        cache["git_user_email"] = None
        cwd = d.getVar("COREBASE") or d.getVar("TOPDIR") or None
        for field, key in (("git_user_name", "user.name"),
                           ("git_user_email", "user.email")):
            try:
                r = subprocess.run(["git", "config", "--get", key], cwd=cwd,
                                   capture_output=True, text=True, timeout=5)
                v = r.stdout.strip()
                if v:
                    cache[field] = v
            except Exception:
                pass

        cache["_done"] = True
        return cache

    # No stashing, no end-of-build aggregation: whatever event fires, shove it
    # at the server right now. The server decides what to do with it.
    ident = yocache_identity()
    payload = {
        "event": e.__class__.__name__,
        "ts": time.time(),
        "build_name": d.getVar("BUILDNAME"),
        "machine": d.getVar("MACHINE"),
        "distro": d.getVar("DISTRO"),
        "hostname": ident.get("hostname"),
        "ip": ident.get("ip"),
        "machine_id": ident.get("machine_id"),
        "git_user_name": ident.get("git_user_name"),
        "git_user_email": ident.get("git_user_email"),
        "pid": os.getpid(),
        "user": d.getVar("USER") or os.environ.get("USER") or get_os_user(),
        "dump": e,
    }

    if isinstance(e, bb.event.MetadataEvent):
        # Only the YOCACHE_METADATA_TYPES we let through above reach here.
        # MissedSstate's _localdata is {'missed': [(fn, task, hash, sstatefile),
        # ...], 'found': [...]}; TaskArtifacts is {'task': ..., 'artifacts': [...]}.
        payload["type"] = getattr(e, "type", None)
        try:
            payload["metadata"] = e._localdata
        except Exception as exc:
            bb.warn("yocache: could not read %s MetadataEvent: %s"
                    % (payload["type"], exc))

    # Sanitize the whole graph up front (bounded, cycle-safe) rather than
    # leaning on json's default= hook, which only sees leaves and trips its
    # own "Circular reference detected" before we can break the cycle.
    body = json.dumps(_jsonable(payload))
    # Key the per-type cap by event class, but split MetadataEvent by its
    # subtype so MissedSstate vs FoundSstate each get their own quota
    # instead of one drowning the other.
    log_key = payload["event"]
    if payload.get("type"):
        log_key = "%s.%s" % (log_key, payload["type"])
    yocache_log(body, log_key)
    yocache_post(d.getVar("YOCACHE_REPORT_ENDPOINT"), body)
}


# Events the handler subscribes to. One per line; see the grouped rationale in
# the header. Override in local.conf to narrow/widen the telemetry.
YOCACHE_EVENTS ??= "\
    bb.event.BuildStarted \
    bb.event.BuildCompleted \
    bb.event.MetadataEvent \
    bb.runqueue.sceneQueueTaskCompleted \
    bb.runqueue.sceneQueueTaskFailed \
    bb.runqueue.taskUniHashUpdate \
    bb.runqueue.runQueueTaskCompleted \
    bb.runqueue.runQueueTaskFailed \
    bb.fetch2.MissingChecksumEvent \
"

addhandler yocache_eventhandler
yocache_eventhandler[eventmask] = "${YOCACHE_EVENTS}"


# --- Upload notify hooks (run in worker processes) -----------------------
# These fire the instant an artifact is produced and do the cheapest possible
# thing: send one line to the cooker uploader's unix socket and return. No
# blocking on the network, no thread (a worker os._exit()s and would kill it).
# All failures are caught — an upload must never break a build.

# After an sstate archive is created and signed, hand it (and its sidecars) to
# the uploader. SSTATE_PKG is the finished blob; runs in SSTATE_BUILDDIR (see
# sstate.bbclass). A fully usable mirror needs the sidecars too: bitbake HEADs
# <pkg>.siginfo on every restore, and <pkg>.sig when sstate is signed.
python yocache_notify_sstate () {
    import os
    try:
        from yocache import uploader
        path = d.getVar("SSTATE_PKG")
        if not (path and os.path.exists(path)):
            bb.warn("yocache: sstate upload missing path: %s" % path)
            return
        sock = d.getVar("YOCACHE_UPLOAD_SOCK")
        sstate_dir = d.getVar("SSTATE_DIR")
        recipe_meta = {var: d.getVar(var) for var in uploader._RECIPE_META_VARS}

        def notify(p):
            # Upload under the SSTATE_DIR-relative path, NOT the bare basename:
            # bitbake stores/fetches sstate as <hash[:2]>/<hash[2:4]>/<file>
            # (generate_sstatefn in sstate.bbclass), and SSTATE_MIRRORS looks it
            # up at exactly that path. Sending the basename drops the two-level
            # prefix, so the GET/HEAD from the mirror never finds the blob. Use
            # the path bitbake itself computed (authoritative) rather than
            # re-deriving the prefix from the hash.
            name = os.path.relpath(p, sstate_dir) if sstate_dir \
                else os.path.basename(p)
            bb.note("yocache: notifying about sstate path: %s, name: %s" % (p, name))
            uploader.notify(sock, "sstate", p, name, recipe_meta=recipe_meta)

        notify(path)

        # .siginfo: sstate_package() dumps this just AFTER SSTATEPOSTCREATEFUNCS
        # (this hook), so it does not exist yet here. Create it the same way
        # sstate is about to — dump_this_task is idempotent, so sstate's later
        # call simply utime()s our file — then upload it. SSTATE_PKG was already
        # finalized by sstate_report_unihash (which ran before this hook), so the
        # path is correct.
        siginfo = path + ".siginfo"
        if not os.path.exists(siginfo):
            bb.siggen.dump_this_task(siginfo, d)
        if os.path.exists(siginfo):
            notify(siginfo)
        else:
            bb.warn("yocache: siginfo still missing: %s" % siginfo)

        # .sig: written by sstate_create_and_sign_package BEFORE this hook, but
        # only for signed sstate (SSTATE_SIG_KEY). Ship it iff it exists.
        sig = path + ".sig"
        if os.path.exists(sig):
            notify(sig)
    except Exception as exc:
        bb.warn("yocache: sstate upload notify failed: %s" % exc)
}
# Register with ':append', NOT '+='. sstate.bbclass hard-assigns
# `SSTATEPOSTCREATEFUNCS = ""` (sstate.bbclass:99) and is inherited via
# INHERIT_DISTRO *after* this class (which arrives through local.conf's INHERIT),
# so a parse-time '+=' here is silently wiped before the create loop in
# sstate_package() ever reads it — the hook would never fire. A ':append' is
# applied at expansion time, after that hard assignment, so it survives.
# Then keep the func out of the task signature (vardep*exclude): a cache layer
# must be transparent, so enabling yocache must not perturb taskhashes/unihashes
# — otherwise yocache builders can't share sstate with non-yocache ones, and
# toggling it forces a full rebuild. Same idiom buildhistory/uninative use for
# the sibling SSTATEPOSTUNPACKFUNCS.
SSTATEPOSTCREATEFUNCS:append = " yocache_notify_sstate"
SSTATEPOSTCREATEFUNCS[vardepvalueexclude] .= "| yocache_notify_sstate"
sstate_package[vardepsexclude] += "yocache_notify_sstate"

# After a recipe's do_fetch, hand every fetched artifact to the uploader:
# the mirror tarballs (git2_*/gitshallow_*) and the plain SRC_URI downloads.
# Reconstructs the Fetch object the same way base_do_fetch does, then notifies
# whichever of each url's localpath/fullmirror/fullshallow exist on disk.
python yocache_notify_dl () {
    import os
    try:
        import bb.fetch2
        from yocache import uploader
        src_uri = (d.getVar("SRC_URI") or "").split()
        if not src_uri:
            return
        sock = d.getVar("YOCACHE_UPLOAD_SOCK")
        recipe_meta = {var: d.getVar(var) for var in uploader._RECIPE_META_VARS}
        fetcher = bb.fetch2.Fetch(src_uri, d)
        seen = set()
        # Bitbake stores recipe-declared checksums as ud.<algo>_expected after
        # verifying them in do_fetch.  These are only meaningful for the actual
        # downloaded file (localpath) — mirror tarballs (fullmirror / fullshallow)
        # are generated by bitbake and have no recipe checksum, so we send nothing
        # for them and let the server compute its own.
        _CHECKSUM_ATTRS = ("sha256", "sha1", "md5", "sha384", "sha512")
        for u in fetcher.urls:
            ud = fetcher.ud[u]
            localpath_checksums = {
                algo: getattr(ud, algo + "_expected", None) or ""
                for algo in _CHECKSUM_ATTRS
            }
            # Drop empty values; if the dict is empty, notify() won't include
            # a "checksums" key and the server marks the blob "locally computed".
            localpath_checksums = {k: v for k, v in localpath_checksums.items() if v}
            for cand in (getattr(ud, "localpath", None),
                         getattr(ud, "fullmirror", None),
                         getattr(ud, "fullshallow", None)):
                if cand and cand not in seen and os.path.isfile(cand):
                    seen.add(cand)
                    cksums = localpath_checksums if cand == ud.localpath else {}
                    uploader.notify(sock, "downloads", cand,
                                    os.path.basename(cand), cksums or None,
                                    recipe_meta=recipe_meta)
    except Exception as exc:
        bb.warn("yocache: dl upload notify failed: %s" % exc)
}
do_fetch[postfuncs] += "yocache_notify_dl"
# Transparency, as on the sstate hook: keep the notify out of do_fetch's
# signature so enabling yocache doesn't change do_fetch taskhashes (and the
# unihashes that depend on them). '+=' is fine here — unlike SSTATEPOSTCREATEFUNCS
# nothing hard-resets the do_fetch[postfuncs] flag after this class.
do_fetch[vardepsexclude] += "yocache_notify_dl"
