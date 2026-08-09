// Mirrors cacheStats in cmd/yocache/stats.go. The *_size string fields have
// omitempty on the Go side and may be absent when the byte count is zero.

export type Stats = {
  downloads_files: number;
  downloads_bytes: number;
  downloads_size?: string;
  sstate_files: number;
  sstate_recipes: number;
  sstate_bytes: number;
  sstate_size?: string;
  hashequiv_taskhashes: number;
  hashequiv_unihashes: number;
  hashequiv_outhashes: number;
};

export type VersionInfo = {
  version: string;
  revision?: string;
  modified?: boolean;
};

export async function fetchStats(): Promise<Stats> {
  const r = await fetch('/api/stats');
  if (!r.ok) throw new Error(`GET /api/stats: ${r.status} ${r.statusText}`);
  return r.json();
}

export async function fetchVersion(): Promise<VersionInfo> {
  const r = await fetch('/version');
  if (!r.ok) throw new Error(`GET /version: ${r.status} ${r.statusText}`);
  return r.json();
}
