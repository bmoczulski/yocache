"""Artifact uploader: push sstate / DL-mirror blobs from a build to yocache.

bitbake has no built-in push path for sstate or download-mirror artifacts, so
yocache.bbclass produces them via two hooks (`SSTATEPOSTCREATEFUNCS` and
`do_fetch[postfuncs]`) and this module ships them to the server. The design is
forced by bitbake's execution model (see notes/sstate-upload-hook.md):

  - The hooks run in short-lived **worker** processes that exit via os._exit(),
    so they can't own a background uploader thread (it'd be killed mid-flight).
    They only do a cheap, local `notify()` over a unix socket and return.

  - The uploader thread lives in the long-lived **cooker** process, started from
    the BuildStarted event handler and drained from BuildCompleted. Worker-fired
    events don't reach bbclass handlers in the cooker, so the worker->cooker
    handoff has to be real IPC (the unix socket), not a bitbake event.

This module is imported (never exec'd inline) precisely so its singleton lives in
sys.modules and survives between the two separate handler invocations.

Roles by process:
  - cooker: start(d) / stop(d) manage the Uploader singleton.
  - worker: notify(sock_path, kind, path, name) — stateless socket client.
"""

import json
import os
import queue
import socket
import threading
import time
import urllib.parse

from .two_phase_put import TwoPhasePut

# Best-effort logging: `bb` exists inside bitbake; fall back to stderr so the
# module is importable (and unit-smokable) standalone.
try:
    import bb  # type: ignore

    def _note(msg):
        bb.note("yocache-upload: " + msg)

    def _warn(msg):
        bb.warn("yocache-upload: " + msg)
except ImportError:  # pragma: no cover - only outside bitbake
    import sys

    def _note(msg):
        print("yocache-upload: " + msg, file=sys.stderr)

    _warn = _note


# Uploader lifecycle states.
IDLE = "idle"
RUNNING = "running"
DRAINING = "draining"

# Sentinel placed on the queue to retire a worker thread.
_SENTINEL = object()

# How long stop() waits for in-flight uploads to finish before giving up.
_DRAIN_TIMEOUT = 120.0

# Maps bitbake checksum algorithm names (as stored in ud.*_expected attributes)
# to the X-Content-* request headers we attach to every PUT that carries them.
# Values are already-verified hex digests; the server can use them later for
# data-consistency checks without recomputing the hash itself.
_CHECKSUM_HEADERS = {
    "sha256": "X-Content-SHA256",
    "sha1":   "X-Content-SHA1",
    "md5":    "X-Content-MD5",
    "sha384": "X-Content-SHA384",
    "sha512": "X-Content-SHA512",
}

# Bitbake variables attached to every PUT as X-BitBake-var-<VARNAME> headers.
# Lets the server tie each blob to the build context it arrived from — useful
# for audit trails, cache pruning by machine/distro, and stale-blob detection.
_BUILD_META_VARS = ("BUILDNAME", "MACHINE", "DISTRO", "DISTRO_VERSION", "TARGET_ARCH")

# Per-blob recipe context forwarded by the worker hooks as X-BitBake-var-*
# headers. Lets the server group and prune blobs by recipe, version, and
# architecture without re-parsing the artifact filename.
_RECIPE_META_VARS = ("PN", "PV", "PR", "BPN", "SSTATE_PKGARCH")

# Cooker-side singleton + the lock guarding its lifecycle transitions.
_uploader = None
_lock = threading.Lock()


class Uploader:
    """Cooker-resident: accepts notifies on a unix socket, PUTs files to yocache.

    One accept thread reads framed `{kind, path, name}` lines off the socket and
    enqueues them; a small worker pool drains the queue and uploads each file.
    Both kinds of failure (bad notify, failed PUT) are logged, never raised — an
    upload must never break a build.
    """

    def __init__(self, sock_path, base_url, threads, skip, build_meta=None, skip_types=None):
        self.sock_path = sock_path
        self.base_url = base_url.rstrip("/")
        self.threads = max(1, int(threads))
        self.skip = skip
        self.skip_types = frozenset(skip_types or ())
        self.build_meta = build_meta or {}
        self.state = IDLE
        self._queue = queue.Queue()
        self._lsock = None
        self._accept_thread = None
        self._workers = []
        self._accepting = False

    # -- lifecycle ---------------------------------------------------------

    def start(self):
        # Fresh listening socket; clear any stale path from a crashed build.
        try:
            os.unlink(self.sock_path)
        except OSError:
            pass
        d = os.path.dirname(self.sock_path)
        if d:
            os.makedirs(d, exist_ok=True)

        self._lsock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self._lsock.bind(self.sock_path)
        self._lsock.listen(64)
        self._lsock.settimeout(1.0)  # so the accept loop can poll _accepting

        self._accepting = True
        self._accept_thread = threading.Thread(
            target=self._accept_loop, name="yocache-upload-accept", daemon=True)
        self._accept_thread.start()

        for i in range(self.threads):
            t = threading.Thread(
                target=self._worker_loop, name="yocache-upload-%d" % i, daemon=True)
            t.start()
            self._workers.append(t)

        self.state = RUNNING
        skip_note = ""
        if self.skip:
            skip_note = ", dry-run"
        elif self.skip_types:
            skip_note = ", skipping %s" % "/".join(sorted(self.skip_types))
        _note("listening on %s -> %s (%d workers%s)" % (
            self.sock_path, self.base_url, self.threads, skip_note))

    def stop(self, timeout=_DRAIN_TIMEOUT):
        if self.state != RUNNING:
            return
        self.state = DRAINING

        # Stop accepting: close the listening socket so no new notifies land.
        # In-flight ones already queued still get uploaded below.
        self._accepting = False
        try:
            self._lsock.close()
        except OSError:
            pass
        if self._accept_thread is not None:
            self._accept_thread.join(timeout=5.0)

        pending = self._queue.qsize()
        if pending:
            _note("draining %d upload(s), up to %.0fs..." % (pending, timeout))

        # Retire workers and wait for them within a shared deadline.
        for _ in self._workers:
            self._queue.put(_SENTINEL)
        deadline = time.monotonic() + timeout
        for t in self._workers:
            t.join(timeout=max(0.0, deadline - time.monotonic()))
        stragglers = [t for t in self._workers if t.is_alive()]
        if stragglers:
            _warn("%d upload(s) still running after %.0fs; leaving them to finish"
                  % (len(stragglers), timeout))

        try:
            os.unlink(self.sock_path)
        except OSError:
            pass
        self._workers = []
        self.state = IDLE
        _note("finished")

    def join(self, timeout=_DRAIN_TIMEOUT):
        """Wait for an in-progress drain to finish (used by the start guard)."""
        deadline = time.monotonic() + timeout
        for t in self._workers:
            t.join(timeout=max(0.0, deadline - time.monotonic()))

    # -- internals ---------------------------------------------------------

    def _accept_loop(self):
        while self._accepting:
            try:
                conn, _ = self._lsock.accept()
            except socket.timeout:
                continue
            except OSError:
                break  # listening socket closed by stop()
            self._handle_conn(conn)

    def _handle_conn(self, conn):
        # One notify per connection: read a single newline-framed JSON object.
        conn.settimeout(5.0)
        buf = b""
        try:
            while b"\n" not in buf and len(buf) < 65536:
                chunk = conn.recv(4096)
                if not chunk:
                    break
                buf += chunk
        except OSError:
            return
        finally:
            try:
                conn.close()
            except OSError:
                pass

        line = buf.split(b"\n", 1)[0].strip()
        if not line:
            return
        try:
            item = json.loads(line)
            kind, path, name = item["kind"], item["path"], item["name"]
            checksums = item.get("checksums") or {}
            recipe_meta = item.get("recipe_meta") or {}
        except (ValueError, KeyError, TypeError) as exc:
            _warn("ignoring malformed notify %r: %s" % (line[:200], exc))
            return
        self._queue.put((kind, path, name, checksums, recipe_meta))

    def _worker_loop(self):
        while True:
            item = self._queue.get()
            try:
                if item is _SENTINEL:
                    return
                self._upload(*item)
            finally:
                self._queue.task_done()

    def _upload(self, kind, path, name, checksums, recipe_meta=None):
        quoted_name = urllib.parse.quote(name)
        url = "%s/%s/%s" % (self.base_url, kind, quoted_name)
        if self.skip:
            _note("dry-run, would PUT %s (%s)" % (url, path))
            return
        if "all" in self.skip_types or kind in self.skip_types:
            _note("skip-type %s, would PUT %s (%s)" % (kind, url, path))
            return
        try:
            size = os.path.getsize(path)
        except OSError as exc:
            _warn("cannot stat %s: %s" % (path, exc))
            return

        req_path = "/%s/%s" % (kind, quoted_name)
        hdrs = {
            "Content-Type": "application/octet-stream",
            "Content-Length": str(size),
            # Only write if the server doesn't already hold this resource.
            # For sstate the URL encodes the unihash, so URL existence
            # implies identical content; for DL the filename is stable
            # enough that the same guard applies. Server responds 412
            # Precondition Failed when the resource exists (RFC 7232 §6);
            # we treat that as a successful no-op, not an error.
            "If-None-Match": "*",
            # Two-phase upload: let the server reject (already has it,
            # conflict, quota) from headers alone, before we ever stream a
            # potentially large blob it doesn't want. See TwoPhasePut and
            # cmd/yocache/upload.go's expectsContinue()/drain guard, written
            # to pair with this.
            "Expect": "100-continue",
        }
        # Attach any already-verified checksums from bitbake so the server
        # can validate what it receives without re-hashing. We trust
        # bitbake's verification; we never compute these ourselves here.
        # Missing keys mean the server computes its own hash and marks it
        # "locally computed" for audit.
        for algo, value in (checksums or {}).items():
            hdr = _CHECKSUM_HEADERS.get(algo)
            if hdr and value:
                hdrs[hdr] = value
        for var, value in self.build_meta.items():
            if value:
                hdrs["X-BitBake-var-" + var] = value
        for var, value in (recipe_meta or {}).items():
            if value:
                hdrs["X-BitBake-var-" + var] = value

        try:
            with open(path, "rb") as fh, TwoPhasePut(self.base_url) as put:
                status, headers, body = put.send(req_path, hdrs, fh)
            self._handle_final(status, headers, url, size, body)
        except Exception as exc:
            _note("PUT %s failed (%s)" % (url, exc))

    def _handle_final(self, status, headers, url, size, body):
        if status == 201:
            _note("PUT %s (%d bytes)" % (url, size))
        elif status == 412:
            _note("skipped %s (server already has it)" % url)
        elif status == 409:
            existing = headers.get("x-yocache-existing-size")
            _note("PUT %s failed (409 conflict): local=%d bytes, existing=%s bytes" %
                  (url, size, existing))
        else:
            # 501 from the current server stub lands here too — expected
            # until storage is implemented; keep it quiet (note, not warn).
            text = body[:200].decode("utf-8", "replace") if body else ""
            _note("PUT %s failed (%s): %s" % (url, status, text))


# -- module-level API (cooker) --------------------------------------------

def start(d):
    """Start the cooker-side uploader for this build. Guards against doubles."""
    global _uploader
    with _lock:
        if _uploader is not None:
            if _uploader.state == RUNNING:
                _note("already running; not starting a second uploader")
                return
            if _uploader.state == DRAINING:
                _note("previous uploader still draining; waiting")
                _uploader.join()
            # IDLE / finished draining -> fall through and recreate.

        sock_path = d.getVar("YOCACHE_UPLOAD_SOCK")
        base_url = d.getVar("YOCACHE_URL") or "http://localhost:6768"
        threads = d.getVar("YOCACHE_UPLOAD_THREADS") or "4"
        skip = bb.utils.to_boolean(d.getVar("YOCACHE_SKIP_UPLOAD"))
        # Normalize "sstate-cache" -> "sstate" so both spellings work.
        raw_types = (d.getVar("YOCACHE_SKIP_UPLOAD_TYPES") or "").split()
        skip_types = {"sstate" if t == "sstate-cache" else t for t in raw_types}
        if not sock_path:
            _warn("YOCACHE_UPLOAD_SOCK unset; uploader not started")
            return

        build_meta = {var: d.getVar(var) for var in _BUILD_META_VARS}
        up = Uploader(sock_path, base_url, threads, skip, build_meta, skip_types)
        try:
            up.start()
        except Exception as exc:
            _warn("could not start uploader: %s" % exc)
            return
        _uploader = up


def stop(d):
    """Drain and stop the cooker-side uploader at end of build."""
    global _uploader
    with _lock:
        if _uploader is None or _uploader.state != RUNNING:
            return
        try:
            _uploader.stop()
        except Exception as exc:
            _warn("error stopping uploader: %s" % exc)


# -- module-level API (worker hooks) --------------------------------------

def notify(sock_path, kind, path, name, checksums=None, recipe_meta=None):
    """Worker-side: hand one artifact to the cooker uploader. Fail-soft.

    A missing/refused socket means uploads are off or not up yet — that's a
    normal condition, so it's logged quietly (note), never as a build warning.

    checksums is an optional {algo: hex_value} dict of already-verified
    checksums (e.g. from SRC_URI recipe flags).  Only non-empty values are
    forwarded; if the dict is empty or None the server will compute its own.

    recipe_meta is an optional {var: value} dict of recipe-level bitbake
    variables (e.g. PN, PV) forwarded to the server as X-BitBake-var-* headers.
    """
    if not sock_path:
        return
    payload = {"kind": kind, "path": path, "name": name}
    if checksums:
        payload["checksums"] = checksums
    if recipe_meta:
        payload["recipe_meta"] = recipe_meta
    msg = json.dumps(payload).encode("utf-8")
    try:
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        s.settimeout(5.0)
        try:
            s.connect(sock_path)
            s.sendall(msg + b"\n")
        finally:
            s.close()
    except OSError as exc:
        _note("notify dropped (%s): %s" % (name, exc))
