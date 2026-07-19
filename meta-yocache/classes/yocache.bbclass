# yocache.bbclass — shared sstate + DL mirror wiring and automatic upload.
#
# Enable per build with:
#     INHERIT += "yocache"
#     YOCACHE_URL = "http://yocache.local:6768"
#
# Inheriting this class wires bitbake's mirrors at the yocache server: it
# prepends PREMIRRORS (source downloads) and prepends SSTATE_MIRRORS (sstate) from
# YOCACHE_URL (see the mirror wiring below), so no separate mirror lines are
# needed in local.conf.
#
# Artifact upload (see the "Artifact upload" section below and
# notes/sstate-upload-hook.md): the SSTATEPOSTCREATEFUNCS / do_fetch[postfuncs]
# hooks notify a cooker-resident uploader thread (lib/yocache/uploader.py) over a
# unix socket, which PUTs each blob to the server the instant it's produced.
#
# A build must never break because of this class: every network/parse failure
# in the mirror wiring, the uploader, and the lifecycle handler below is caught
# and downgraded to a bb.warn/bb.note.

# Base URL of the yocache server. The canonical place to set this is
# local.conf / site.conf; this weak default just keeps the class self-contained.
YOCACHE_URL ??= "http://localhost:6768"

# All replacements defined in build_mirroruris() in bitbake/lib/bb/fetch2/__init__.py
#   TYPE       origud.type        e.g. git / https / crate / gitsm
#   HOST       origud.host        e.g. git.openembedded.org
#   PATH       origud.path        leading-slash repo/file path
#   BASENAME   path.split('/')[-1]
#   MIRRORNAME host+path flattened (':'->'.', '/'->'.', '*'->'.')
# YOCACHE_URL_REPLACEMENTS = "type=TYPE/host=HOST/mirrorname=MIRRORNAME/basename=BASENAME/path=PATH/"
YOCACHE_URL_REPLACEMENTS = "PATH"

# --- DL (downloads) + sstate mirror wiring --------------------------------
# Composed once here; the inc files below use them via ${...} expansion so the
# URL is not repeated per-protocol. Direct PREMIRRORS prepend avoids touching
# SOURCE_MIRROR_URL, which collapses multi-URL mirrors into 3-token lines that
# bitbake rejects with "should have paired members".
#
# Identity prefix: the server can't read X-* headers from bitbake's internal
# mirror fetcher, so build context is embedded as /key/value/ path segments
# before the kind sentinel ("sstate" or "downloads"). All keys are optional;
# missing values are silently omitted. The prefix is signature-neutral —
# SSTATE_MIRRORS and PREMIRRORS never enter a task hash (verified in sstate.bbclass).
def _yocache_identity_prefix(d):
    parts = []
    for key, var in [("machine", "MACHINE"), ("buildname", "BUILDNAME")]:
        val = (d.getVar(var) or "").strip()
        if val:
            parts += [key, val]
    return "/".join(parts) + "/" if parts else ""

YOCACHE_DL_URL = "${YOCACHE_URL}/${@_yocache_identity_prefix(d)}downloads/${YOCACHE_URL_REPLACEMENTS}"
YOCACHE_SS_URL = "${YOCACHE_URL}/${@_yocache_identity_prefix(d)}sstate/${YOCACHE_URL_REPLACEMENTS};downloadfilename=PATH"

def _yocache_skip_fetch(d, kind):
    types = (d.getVar("YOCACHE_SKIP_FETCH_TYPES") or "").split()
    return "all" in types or kind in types or ("sstate-cache" in types and kind == "sstate")

# Build PREMIRRORS dynamically: only include protocols whose fetch2 module is
# present in this bitbake version. cvs/bzr/osc were dropped in modern bitbake;
# rather than hard-coding which version removed them, probe via importlib so
# this adapts automatically as modules come and go.
def _yocache_premirrors(d):
    import importlib
    dl_url = d.getVar("YOCACHE_DL_URL")
    # (premirror pattern, bb.fetch2.<module> that handles it)
    candidates = [
        ("svn://.*/.*",    "svn"),
        ("git://.*/.*",    "git"),
        ("gitsm://.*/.*",  "gitsm"),
        ("hg://.*/.*",     "hg"),
        ("p4://.*/.*",     "perforce"),
        ("https?://.*/.*", "wget"),
        ("ftp://.*/.*",    "wget"),
        ("npm://.*/?.*",   "npm"),
        # Legacy protocols removed in modern bitbake — included only if present.
        ("cvs://.*/.*",    "cvs"),
        ("bzr://.*/.*",    "bzr"),
        ("osc://.*/.*",    "osc"),
    ]
    seen = {}
    lines = []
    for pattern, module in candidates:
        if module not in seen:
            try:
                importlib.import_module("bb.fetch2." + module)
                seen[module] = True
            except ImportError:
                seen[module] = False
        if seen[module]:
            lines.append("%s    %s" % (pattern, dl_url))
    return " \\n ".join(lines) + " \\n "

YOCACHE_PREMIRRORS = "${@'' if _yocache_skip_fetch(d, 'downloads') else _yocache_premirrors(d)}"
YOCACHE_SS_MIRRORS = "${@'' if _yocache_skip_fetch(d, 'sstate') else 'file://.* ' + d.getVar('YOCACHE_SS_URL') + ' \\n '}"

# --- Per-recipe task-time ledger -------------------------------------------
# Backs the build-time attribution described near yocache_task_started
# below: a plain-text, append-only file (one "<task> <ms>" line per real
# task completion) under ${T}, so a downstream sstate task can credit
# itself with the wall-clock time of every upstream (non-sstate) task that
# fed it, not just its own. ${T} = ${WORKDIR}/temp, and bitbake's own
# _exec_task creates it unconditionally for every task before TaskStarted
# even fires, so there's no directory race to guard against here.
#
# Locking uses a dedicated sibling path (<ledger>.lock), never the ledger
# file itself: bb.utils.lockfile() opens whatever path it's given and holds
# its own fd on it for the lock's duration, so using a separate path keeps
# that fd completely independent of this code's own open() calls on the
# ledger's actual content.
def _yocache_ledger_path(d):
    import os
    return os.path.join(d.getVar("T"), "yocache-tasktimes.log")

# Append one task's own elapsed ms, tagged with this invocation's BUILDNAME.
# O(1): no read, no parse, no rewrite of existing content — cheap enough to
# call on every single task completion in the build. The BUILDNAME tag is
# what lets a sweep (below) tell "upstream work from THIS build" apart from
# a leftover from an earlier, unrelated invocation of the same recipe in the
# same WORKDIR (e.g. a partial/forced rebuild — "bitbake -c compile -f" —
# that skips do_fetch entirely because its stamp is still valid, so nothing
# ever resets the ledger between invocations).
def _yocache_ledger_write(d, task, ms):
    path = _yocache_ledger_path(d)
    buildname = d.getVar("BUILDNAME") or ""
    lf = bb.utils.lockfile(path + ".lock")
    if lf is None:
        return
    try:
        with open(path, "a") as f:
            f.write("%s %s %d\n" % (buildname, task, ms))
    except Exception as exc:
        bb.note("yocache: ledger append failed: %s" % exc)
    finally:
        bb.utils.unlockfile(lf)

# Sum only the lines tagged with THIS invocation's BUILDNAME, then truncate
# the ledger to empty so the next sstate task to consume it starts from
# zero. A line tagged with a different (or missing) BUILDNAME is a leftover
# from an earlier, unrelated invocation — silently ignored, never credited
# to this build, and flushed out by the truncate below regardless. A
# malformed line (e.g. a torn write from a crash) is skipped the same way —
# telemetry must never break a build. Returns 0 on any failure.
def _yocache_ledger_sweep_and_clear(d):
    path = _yocache_ledger_path(d)
    buildname = d.getVar("BUILDNAME") or ""
    lf = bb.utils.lockfile(path + ".lock")
    if lf is None:
        return 0
    total = 0
    try:
        try:
            with open(path, "r") as f:
                for line in f:
                    parts = line.split()
                    if len(parts) == 3 and parts[0] == buildname:
                        try:
                            total += int(parts[2])
                        except ValueError:
                            pass
        except FileNotFoundError:
            pass
        with open(path, "w"):
            pass  # truncate — also flushes out any foreign-BUILDNAME lines
    except Exception as exc:
        bb.note("yocache: ledger sweep failed: %s" % exc)
    finally:
        bb.utils.unlockfile(lf)
    return total

# Dunfell..Honister require _prepend; Kirkstone+ require :prepend.
# LAYERSERIES_CORENAMES is set by meta/conf/layer.conf and is available here.
require classes/${@'yocache-mirrors-compat.inc' if bb.utils.filter('LAYERSERIES_CORENAMES', 'dunfell gatesgarth hardknott honister', d) else 'yocache-mirrors-new.inc'}

python () {
    # SSTATEPOSTCREATEFUNCS: must be set via d.appendVar, not a text operator.
    # sstate.bbclass hard-assigns SSTATEPOSTCREATEFUNCS = "" at parse time;
    # this anonymous function runs after that, so d.appendVar() survives to
    # expansion time when sstate_package() reads the variable.
    d.appendVar("SSTATEPOSTCREATEFUNCS", " yocache_notify_sstate")
}

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

# Per-type upload opt-out: space-separated list of artifact types to skip.
# Valid values: "sstate" (or "sstate-cache"), "downloads", "all". Example:
#   YOCACHE_SKIP_UPLOAD_TYPES = "downloads"   # fetch from cache but don't push DL artifacts
#   YOCACHE_SKIP_UPLOAD_TYPES = "sstate"      # only suppress sstate uploads
#   YOCACHE_SKIP_UPLOAD_TYPES = "all"         # upload nothing (shortcut for "sstate downloads")
YOCACHE_SKIP_UPLOAD_TYPES ??= ""

# Per-type fetch opt-out: omit the relevant PREMIRRORS / SSTATE_MIRRORS entry so
# bitbake never tries yocache for that artifact type. Use on CI builders that
# should populate the cache but never read from it (cold builds must stay cold).
# Valid values: "sstate" (or "sstate-cache"), "downloads", "all". Example:
#   YOCACHE_SKIP_FETCH_TYPES = "downloads"    # fetch sstate from cache; never DL from it
#   YOCACHE_SKIP_FETCH_TYPES = "sstate"       # fetch DL from cache; never sstate from it
#   YOCACHE_SKIP_FETCH_TYPES = "all"          # populate-only; never read from yocache
YOCACHE_SKIP_FETCH_TYPES ??= ""

# Space-separated list of recipe names (PN) to exclude from all cache uploads.
# A build-side complement to the server's --block-recipe flag: recipes known to
# produce non-reproducible or broken artifacts are silently skipped before any
# socket or network I/O rather than being rejected after the fact at the server.
YOCACHE_BLOCK_RECIPES ??= ""

# Size of the cooker uploader's PUT worker pool.
YOCACHE_UPLOAD_THREADS ??= "4"

# Make the git mirror tarballs actually exist so there's something to upload:
# without these, DL_DIR holds only the bare git2/<...> clone dir, no tarball
# (see notes/git-mirror-tarballs.md). Weak so a build can still opt out.
BB_GENERATE_MIRROR_TARBALLS ??= "1"
BB_GENERATE_SHALLOW_TARBALLS ??= "1"

python yocache_build_lifecycle () {
    import json
    import urllib.parse
    import urllib.request

    d = e.data

    import sys
    _yclib = d.getVar('YOCACHE_LAYER_LIBDIR')
    if _yclib and _yclib not in sys.path:
        sys.path.insert(0, _yclib)

    def yocache_print_build_stats():
        # One-line "yocache helped you" / "you helped your teammates" summary,
        # printed once at BuildCompleted next to bitbake's own built-in
        # "Sstate summary: Wanted ... Mirrors ..." line. Fetched from
        # GET /api/build-stats/<BUILDNAME> only after uploader.stop(d) above
        # has drained, so this build's own final uploads are already
        # reflected in the server's numbers. Best-effort like everything else
        # here: a fetch/parse failure is a bb.note, never a build problem.
        buildname = d.getVar("BUILDNAME")
        base_url = d.getVar("YOCACHE_URL")
        if not (buildname and base_url):
            bb.note("yocache: build stats summary skipped (BUILDNAME=%r YOCACHE_URL=%r)"
                    % (buildname, base_url))
            return
        endpoint = "%s/api/build-stats/%s" % (base_url, urllib.parse.quote(buildname, safe=""))
        try:
            with urllib.request.urlopen(endpoint, timeout=5) as resp:
                stats = json.loads(resp.read().decode("utf-8"))
        except Exception as exc:
            bb.note("yocache: could not fetch build stats: %s" % exc)
            return

        def _hms(seconds):
            h, rem = divmod(int(seconds or 0), 3600)
            m, s = divmod(rem, 60)
            return "%02d:%02d:%02d" % (h, m, s)

        def _object_bits(ss, dl):
            # Only mention a category if it actually has something to report,
            # so a build that only touched sstate (or only downloads) doesn't
            # get a misleading "0 download object(s)" tacked on.
            bits = []
            if ss.get("count", 0):
                bits.append("%d sstate" % ss["count"])
            if dl.get("count", 0):
                bits.append("%d download" % dl["count"])
            return bits

        up_ss = stats.get("uploads", {}).get("sstate", {})
        up_dl = stats.get("uploads", {}).get("downloads", {})
        dn_ss = stats.get("downloads", {}).get("sstate", {})
        dn_dl = stats.get("downloads", {}).get("downloads", {})

        clauses = []
        reused_bits = _object_bits(dn_ss, dn_dl)
        if reused_bits:
            clause = "reused %s object(s)" % " + ".join(reused_bits)
            if dn_ss.get("count", 0):
                clause += ", saving ~%s of rebuild time" % _hms(dn_ss.get("seconds", 0))
            clauses.append(clause)
        contributed_bits = _object_bits(up_ss, up_dl)
        if contributed_bits:
            clause = "contributed %s object(s)" % " + ".join(contributed_bits)
            if up_ss.get("count", 0):
                clause += ", worth ~%s to your teammates, you rock! ❤️" % _hms(up_ss.get("seconds", 0))
            clauses.append(clause)

        # Nothing on either side (e.g. a build with no cache interaction at
        # all) — say nothing rather than an empty "yocache summary:" line.
        if not clauses:
            clauses.append("not used in this build")
        bb.plain("yocache summary: " + " | ".join(clauses))

    # Start the cooker-resident artifact uploader for this build. This
    # handler runs in the cooker (BuildStarted fires there), the only place
    # a thread can outlive a task. Fail-soft: an upload must never break a
    # build.
    if isinstance(e, bb.event.BuildStarted):
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
        try:
            yocache_print_build_stats()
        except Exception as exc:
            bb.warn("yocache: could not print build stats summary: %s" % exc)
}

addhandler yocache_build_lifecycle
yocache_build_lifecycle[eventmask] = "bb.event.BuildStarted bb.event.BuildCompleted"


# --- Upload notify hooks (run in worker processes) -----------------------
# These fire the instant an artifact is produced and do the cheapest possible
# thing: send one line to the cooker uploader's unix socket and return. No
# blocking on the network, no thread (a worker os._exit()s and would kill it).
# All failures are caught — an upload must never break a build.

# Wall-clock task timing, so an uploaded sstate artifact can carry "this took
# N seconds to build" to the server — including the upstream, non-sstate
# tasks (do_fetch/do_unpack/do_patch/do_configure/do_compile/do_install, none
# of which are ever in SSTATETASKS) that a cache hit on THIS artifact also
# lets bitbake skip. That upstream time can't be measured inside a single
# task the way buildstats.bbclass measures its own (classes-global/
# buildstats.bbclass: stash start on TaskStarted, read back on TaskSucceeded
# in the same forked worker process) — it has to be carried across several
# different tasks' separate forked processes, i.e. persisted to disk. See
# the per-recipe task-time ledger helpers above (_yocache_ledger_*).
#
# The mechanism, in three pieces:
#   1. yocache_task_started (below): stash this task's own start time, same
#      as before. Every ledger line is tagged with BUILDNAME at write time
#      (see _yocache_ledger_write), so — unlike an earlier version of this
#      mechanism — there's no need to guess when a build is "starting over"
#      (e.g. by resetting on do_fetch): a partial/forced rebuild that skips
#      do_fetch (its stamp still valid) gets its own fresh BUILDNAME anyway,
#      so anything it writes is automatically distinguishable from — and
#      never mixed with — a leftover from an earlier, unrelated invocation
#      of the same recipe in the same WORKDIR.
#   2. yocache_task_finished (bb.build.TaskSucceeded, new): for every real
#      task, appends its own elapsed ms to the ledger — UNLESS the stash is
#      already gone, which happens whenever this same task is itself
#      sstate-cacheable and yocache_notify_sstate (below) already consumed
#      it. That single consume-on-read (d.delVar) is what stops a task's
#      own time from being reported twice — once directly by its own sstate
#      upload, and again by whichever later task sweeps the ledger. It also
#      means no SSTATETASKS lookup is needed here to know "this task
#      already reported itself". _setscene tasks (cache-hit restores) are
#      excluded explicitly instead: sstate.bbclass never wraps them with
#      sstate_task_prefunc/postfunc, so their stash is never consumed by
#      step 3, and restore latency must not be credited as rebuild time.
#   3. yocache_notify_sstate (below): sweeps and clears the ledger, crediting
#      only this invocation's own BUILDNAME-tagged lines (anything else is a
#      foreign leftover, ignored but still flushed out by the clear), and
#      adds that total to its own task's elapsed time. Whichever sstate
#      sibling gets there first (e.g. do_populate_sysroot vs. do_package,
#      which can run in genuinely parallel forked processes) claims the
#      whole currently-unclaimed pool; the loser finds it already empty.
#      Because an ancestor task's TaskSucceeded (hence its ledger append)
#      always completes before any descendant even starts, the only real
#      race is sibling vs. sibling, which the ledger's lock resolves — each
#      millisecond is claimed exactly once.
python yocache_task_started () {
    d.setVar("__yocache_task_started", e.time)
}
addhandler yocache_task_started
yocache_task_started[eventmask] = "bb.build.TaskStarted"

# Records every real task's own elapsed time into the per-recipe ledger, for
# a later sstate task to absorb. See the three-piece mechanism described
# above yocache_task_started. Deliberately its own addhandler, separate from
# yocache_build_lifecycle: local bookkeeping only, fires on a task event
# neither of that handler's two subscriptions cover.
python yocache_task_finished () {
    import time
    task = getattr(e, "task", None)
    if not task or task.endswith("_setscene"):
        return
    started = d.getVar("__yocache_task_started", False)
    if started is None:
        # Either never stashed, or already consumed by this same task's own
        # yocache_notify_sstate (it deletes the stash right after using it)
        # — either way, nothing new to credit.
        return
    ms = int(round((time.time() - started) * 1000))
    _yocache_ledger_write(d, task, ms)
}
addhandler yocache_task_finished
yocache_task_finished[eventmask] = "bb.build.TaskSucceeded"

# After an sstate archive is created and signed, hand it (and its sidecars) to
# the uploader. SSTATE_PKG is the finished blob; runs in SSTATE_BUILDDIR (see
# sstate.bbclass). A fully usable mirror needs the sidecars too: bitbake HEADs
# <pkg>.siginfo on every restore, and <pkg>.sig when sstate is signed.
python yocache_notify_sstate () {
    import os, sys, time
    _yclib = d.getVar('YOCACHE_LAYER_LIBDIR')
    if _yclib and _yclib not in sys.path:
        sys.path.insert(0, _yclib)
    try:
        _blocked = (d.getVar("YOCACHE_BLOCK_RECIPES") or "").split()
        if _blocked and (d.getVar("PN") or "") in _blocked:
            bb.note("yocache: skipping sstate upload for blocked recipe %s" % d.getVar("PN"))
            return
        from yocache import uploader
        path = d.getVar("SSTATE_PKG")
        if not (path and os.path.exists(path)):
            bb.warn("yocache: sstate upload missing path: %s" % path)
            return

        # Wall-clock time this task took to run, if yocache_task_started caught
        # its start above, PLUS whatever upstream (non-sstate) task time this
        # sstate object also lets bitbake skip (see the ledger mechanism
        # described above yocache_task_started). Stashed as a plain datastore
        # var so it flows out through recipe_meta below, exactly like
        # PN/PV/etc., and reaches the server as the
        # X-BitBake-var-YOCACHE_BUILD_MS PUT header. Milliseconds, not whole
        # seconds: most sstate-producing tasks (e.g. admin/packaging steps,
        # *-native quick builds) finish in well under a second, and
        # int()-truncating to whole seconds would report 0 for every one of
        # them.
        _started = d.getVar("__yocache_task_started", False)
        _now = time.time()
        if _started is not None:
            # Consume the stash: this is what tells yocache_task_finished
            # (bb.build.TaskSucceeded, fires after this postfunc) that this
            # task's own time has already been reported here, so it must not
            # write it into the ledger too — that would let a later sstate
            # task absorb it a second time.
            d.delVar("__yocache_task_started")
            own_ms = int(round((_now - _started) * 1000))
            absorbed_ms = _yocache_ledger_sweep_and_clear(d)
            d.setVar("YOCACHE_BUILD_MS", str(own_ms + absorbed_ms))

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
# The hook registration (d.appendVar in the anonymous python () above) must
# not perturb taskhashes/unihashes — a cache layer must be transparent, so
# enabling yocache must not force a full rebuild or prevent sstate sharing
# with non-yocache builds.  The vardep*exclude flags below achieve that; they
# use [flag] syntax which is stable across all bitbake versions.
SSTATEPOSTCREATEFUNCS[vardepvalueexclude] .= "| yocache_notify_sstate"
sstate_package[vardepsexclude] += "yocache_notify_sstate"

# After a recipe's do_fetch, hand every fetched artifact to the uploader:
# the mirror tarballs (git2_*/gitshallow_*) and the plain SRC_URI downloads.
# Reconstructs the Fetch object the same way base_do_fetch does, then notifies
# whichever of each url's localpath/fullmirror/fullshallow exist on disk.
python yocache_notify_dl () {
    import os, sys
    _yclib = d.getVar('YOCACHE_LAYER_LIBDIR')
    if _yclib and _yclib not in sys.path:
        sys.path.insert(0, _yclib)
    try:
        _blocked = (d.getVar("YOCACHE_BLOCK_RECIPES") or "").split()
        if _blocked and (d.getVar("PN") or "") in _blocked:
            bb.note("yocache: skipping downloads upload for blocked recipe %s" % d.getVar("PN"))
            return
        from bb import fetch2
        from yocache import uploader
        src_uri = (d.getVar("SRC_URI") or "").split()
        if not src_uri:
            return
        sock = d.getVar("YOCACHE_UPLOAD_SOCK")
        recipe_meta = {var: d.getVar(var) for var in uploader._RECIPE_META_VARS}
        fetcher = fetch2.Fetch(src_uri, d)
        seen = set()
        # Bitbake stores recipe-declared checksums as ud.<algo>_expected after
        # verifying them in do_fetch.  These are only meaningful for the actual
        # downloaded file (localpath) — mirror tarballs (fullmirror / fullshallow)
        # are generated by bitbake and have no recipe checksum, so we send nothing
        # for them and let the server compute its own.
        _CHECKSUM_ATTRS = ("sha256", "sha1", "md5", "sha384", "sha512")
        for u in fetcher.urls:
            ud = fetcher.ud[u]
            if ud.type == "file":
                continue
            # Repack the full mirror tarball if the local bare clone is newer.
            # Without this, a builder that warms from yocache and then fetches
            # upstream updates will never refresh the tarball (bitbake's
            # build_mirror_data guard is "not os.path.exists(fullmirror)").
            if (getattr(ud, 'write_tarballs', False)
                    and getattr(ud, 'fullmirror', None)
                    and getattr(ud, 'localpath', None)
                    and os.path.isfile(ud.fullmirror)
                    and os.path.exists(ud.localpath)
                    and os.path.getmtime(ud.localpath) > os.path.getmtime(ud.fullmirror)):
                _tmp = ud.fullmirror + ".yocache-repack"
                try:
                    os.rename(ud.fullmirror, _tmp)
                    ud.method.build_mirror_data(ud, d)
                    bb.note("yocache: repacked stale mirror tarball %s" % os.path.basename(ud.fullmirror))
                except Exception as _e:
                    bb.warn("yocache: repack of %s failed: %s" % (os.path.basename(ud.fullmirror), _e))
                    if os.path.exists(_tmp) and not os.path.exists(ud.fullmirror):
                        os.rename(_tmp, ud.fullmirror)
                else:
                    if os.path.exists(_tmp):
                        os.remove(_tmp)
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
