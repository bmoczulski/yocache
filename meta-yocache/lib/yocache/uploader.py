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
import urllib.error
import urllib.parse
import urllib.request

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

    def __init__(self, sock_path, base_url, threads, skip):
        self.sock_path = sock_path
        self.base_url = base_url.rstrip("/")
        self.threads = max(1, int(threads))
        self.skip = skip
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
        _note("listening on %s -> %s (%d workers%s)" % (
            self.sock_path, self.base_url, self.threads,
            ", dry-run" if self.skip else ""))

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
            _note("draining %d queued upload(s)" % pending)

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
        _note("stopped")

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
        except (ValueError, KeyError, TypeError) as exc:
            _warn("ignoring malformed notify %r: %s" % (line[:200], exc))
            return
        self._queue.put((kind, path, name))

    def _worker_loop(self):
        while True:
            item = self._queue.get()
            try:
                if item is _SENTINEL:
                    return
                self._upload(*item)
            finally:
                self._queue.task_done()

    def _upload(self, kind, path, name):
        url = "%s/%s/%s" % (self.base_url, kind, urllib.parse.quote(name))
        if self.skip:
            _note("dry-run, would PUT %s (%s)" % (url, path))
            return
        try:
            size = os.path.getsize(path)
        except OSError as exc:
            _warn("cannot stat %s: %s" % (path, exc))
            return
        try:
            with open(path, "rb") as fh:
                req = urllib.request.Request(
                    url, data=fh, method="PUT",
                    headers={
                        "Content-Type": "application/octet-stream",
                        "Content-Length": str(size),
                        # Only write if the server doesn't already hold this
                        # resource. For sstate the URL encodes the unihash, so
                        # URL existence implies identical content; for DL the
                        # filename is stable enough that the same guard applies.
                        # Server responds 412 Precondition Failed when the
                        # resource exists (RFC 7232 §6); we treat that as a
                        # successful no-op, not an error.
                        "If-None-Match": "*",
                    })
                with urllib.request.urlopen(req, timeout=300) as resp:
                    resp.read()
            _note("PUT %s (%d bytes)" % (url, size))
        except urllib.error.HTTPError as exc:
            if exc.code == 412:
                _note("skipped %s (server already has it)" % url)
            else:
                # 501 from the current server stub lands here too — expected
                # until storage is implemented; keep it quiet (note, not warn).
                _note("PUT %s failed (%s)" % (url, exc))
        except Exception as exc:
            _note("PUT %s failed (%s)" % (url, exc))


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
        if not sock_path:
            _warn("YOCACHE_UPLOAD_SOCK unset; uploader not started")
            return

        up = Uploader(sock_path, base_url, threads, skip)
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

def notify(sock_path, kind, path, name):
    """Worker-side: hand one artifact to the cooker uploader. Fail-soft.

    A missing/refused socket means uploads are off or not up yet — that's a
    normal condition, so it's logged quietly (note), never as a build warning.
    """
    if not sock_path:
        return
    msg = json.dumps({"kind": kind, "path": path, "name": name}).encode("utf-8")
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
