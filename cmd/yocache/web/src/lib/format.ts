// SI-decimal byte formatter matching humanize.Bytes on the Go side
// (dustin/go-humanize), so figures in the dashboard read the same as the
// startup log line and /api/stats *_size fields.

const UNITS = ['B', 'kB', 'MB', 'GB', 'TB', 'PB', 'EB'];

export function humanBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return '0 B';
  if (bytes < 1000) return `${bytes} B`;

  let i = 0;
  let v = bytes;
  while (v >= 1000 && i < UNITS.length - 1) {
    v /= 1000;
    i++;
  }
  return `${v.toFixed(v < 10 ? 2 : v < 100 ? 1 : 0)} ${UNITS[i]}`;
}

export function humanCount(n: number): string {
  return n.toLocaleString();
}

// Seconds (as the hash-equivalence get-stats reply reports them) in the
// largest unit that keeps the figure >= 1: requests are typically µs to ms.
export function humanSeconds(s: number): string {
  if (!Number.isFinite(s) || s <= 0) return '0 s';
  const [v, unit] =
    s >= 1 ? [s, 's'] : s >= 1e-3 ? [s * 1e3, 'ms'] : [s * 1e6, 'µs'];
  return `${v.toFixed(v < 10 ? 2 : v < 100 ? 1 : 0)} ${unit}`;
}
