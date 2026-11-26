// Minimal OEHASHEQUIV client over the server's /hashequiv WebSocket — just
// enough to ask get-stats, the same command bitbake-hashclient uses, so the
// dashboard reads the very figures a bitbake client would rather than a
// second HTTP copy of them. Framing per cmd/yocache/hashequiv.go: one text
// frame per line, handshake "OEHASHEQUIV <ver>" then an empty frame ending
// the (absent) headers, then one JSON frame per request/reply.

// Mirrors hashEquivStatsSnapshot in cmd/yocache/hashequiv_stats.go. Times are
// in seconds.
export type HashEquivRequestStats = {
  num: number;
  total_time: number;
  max_time: number;
  average: number;
  stdev: number;
};

const TIMEOUT_MS = 5000;

// Resolved relative to the dashboard at <prefix>/ui/, like api.ts, so a
// reverse-proxy prefix is kept; http(s) maps to ws(s).
function hashEquivURL(): string {
  const u = new URL('../hashequiv', location.href);
  u.protocol = u.protocol === 'https:' ? 'wss:' : 'ws:';
  return u.toString();
}

export function fetchHashEquivStats(): Promise<HashEquivRequestStats> {
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(hashEquivURL());
    const fail = (msg: string) => {
      clearTimeout(timer);
      ws.close(1000);
      reject(new Error(msg));
    };
    const timer = setTimeout(() => fail('hash-equivalence get-stats timed out'), TIMEOUT_MS);

    ws.onopen = () => {
      ws.send('OEHASHEQUIV 1.1');
      ws.send('');
      ws.send(JSON.stringify({ 'get-stats': null }));
    };
    ws.onmessage = (ev) => {
      clearTimeout(timer);
      ws.close(1000);
      try {
        const reply = JSON.parse(String(ev.data));
        if (!reply?.requests) throw new Error(`unexpected reply: ${ev.data}`);
        resolve(reply.requests);
      } catch (e) {
        reject(e instanceof Error ? e : new Error(String(e)));
      }
    };
    // What --hash-equivalence off looks like: /hashequiv isn't routed, so the
    // upgrade is refused. The browser hides the status code, so a proxy that
    // doesn't pass WebSockets lands here too; off is by far the likelier one.
    ws.onerror = () => fail('not enabled on this server');
  });
}
