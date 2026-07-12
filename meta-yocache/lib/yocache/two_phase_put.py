"""A single HTTP PUT using the "Expect: 100-continue" two-phase protocol:
send headers, let the server decide whether it wants the body before ever
streaming it.

http.client's own getresponse() can't be used to make that decision: its
begin() transparently loops past a leading "100 Continue" status line
looking for a final one, which would deadlock here (the server won't
finalize until it has the body, and the body won't be sent until told to).
So the first response is parsed by hand instead, via an unbuffered raw
reader (buffering=0 -- a buffered one could pull bytes belonging to a later
parse and lose them once discarded). This works identically for a plain
socket and an ssl.SSLSocket (https:// endpoints, e.g. behind a
TLS-terminating facade): SSLSocket overrides recv_into(), what makefile()
relies on, to decrypt through the TLS layer, with no flags argument
involved -- unlike SSLSocket.recv(), which rejects MSG_PEEK, ruling out a
peek-based shortcut for https://.
"""

import http.client
import urllib.parse


class TwoPhasePut:
    """PUT a file-like body to path over a two-phase Expect: 100-continue
    exchange. One instance is good for exactly one request; use as a
    context manager so the connection is always closed:

        with TwoPhasePut(base_url) as put:
            status, headers, body = put.send(path, headers, fh)
    """

    def __init__(self, base_url, timeout=300):
        parsed = urllib.parse.urlsplit(base_url)
        conn_cls = (http.client.HTTPSConnection if parsed.scheme == "https"
                    else http.client.HTTPConnection)
        port = parsed.port or (443 if parsed.scheme == "https" else 80)
        self._conn = conn_cls(parsed.hostname, port, timeout=timeout)

    def __enter__(self):
        return self

    def __exit__(self, exc_type, exc, tb):
        self._conn.close()
        return False

    def send(self, path, headers, fh):
        """PUT fh's contents to path. headers must include Content-Length
        and Expect: 100-continue for the two-phase behavior to apply.
        Returns (status, headers, body): headers keyed by lowercase name,
        body the raw bytes carried by whichever response was final."""
        self._conn.connect()
        self._conn.putrequest("PUT", path)
        for k, v in headers.items():
            self._conn.putheader(k, v)
        self._conn.endheaders()  # headers only -- body not sent yet

        raw = self._conn.sock.makefile("rb", buffering=0)
        status, resp_headers = self._read_status_and_headers(raw)

        if status != 100:
            return status, resp_headers, self._read_response_body(raw, resp_headers)

        while True:
            chunk = fh.read(65536)
            if not chunk:
                break
            self._conn.send(chunk)
        resp = self._conn.getresponse()
        body = resp.read()
        return resp.status, {k.lower(): v for k, v in resp.getheaders()}, body

    # -- private: hand-rolled HTTP/1.1 first-response parsing ---------------

    def _read_line(self, raw):
        line = raw.readline(8192)
        if not line:
            raise ConnectionError("connection closed while reading response")
        return line.rstrip(b"\r\n")

    def _read_status_and_headers(self, raw):
        status_line = self._read_line(raw)
        status = int(status_line.split(b" ", 2)[1])
        headers = {}
        while True:
            line = self._read_line(raw)
            if not line:
                break
            name, _, value = line.partition(b":")
            headers[name.strip().lower().decode("ascii", "replace")] = (
                value.strip().decode("iso-8859-1", "replace"))
        return status, headers

    def _read_chunked_body(self, raw):
        body = bytearray()
        while True:
            size_line = self._read_line(raw)
            size = int(size_line.split(b";", 1)[0], 16)  # ignore chunk-ext
            if size == 0:
                while self._read_line(raw):  # consume trailer headers, if any
                    pass
                break
            body += raw.read(size)
            self._read_line(raw)  # consume the chunk's trailing CRLF
        return bytes(body)

    def _read_response_body(self, raw, headers):
        """Read whatever body accompanies an already-parsed status+header
        block, honoring all three HTTP/1.1 framing modes (RFC 9112 S6.3):
        chunked, Content-Length, or (absent both) read until connection
        close. The close-delimited fallback is safe here because this
        connection is discarded right after, in every branch -- never
        reused. Deliberately server-agnostic: no assumption about what the
        server's current responses happen to look like, since that may
        change."""
        if "chunked" in headers.get("transfer-encoding", "").lower():
            return self._read_chunked_body(raw)
        length = headers.get("content-length")
        if length is not None:
            return raw.read(int(length))
        return raw.read()  # neither header: body ends at connection close
