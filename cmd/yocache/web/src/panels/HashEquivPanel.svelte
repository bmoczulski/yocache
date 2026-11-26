<script lang="ts">
  import type { Stats } from '../lib/api';
  import type { HashEquivRequestStats } from '../lib/hashequiv';
  import { humanCount, humanSeconds } from '../lib/format';

  // requests is null while get-stats is in flight; requestsError is set
  // instead when it failed (e.g. --hash-equivalence off). Row counts come
  // from /api/stats and are shown either way.
  type Props = {
    stats: Stats;
    requests: HashEquivRequestStats | null;
    requestsError: string | null;
  };
  let { stats, requests, requestsError }: Props = $props();

  // Taskhashes beyond one per unihash are ones whose output matched an
  // existing unihash — each a task whose sstate was reused instead of rebuilt.
  const spared = $derived(
    Math.max(0, stats.hashequiv_taskhashes - stats.hashequiv_unihashes),
  );
</script>

<section class="panel">
  <header>
    <h2>Hash equivalence</h2>
    <span class="muted">{humanCount(spared)} taskhashes resolved to an equivalent unihash</span>
  </header>

  <dl class="grid">
    <div>
      <dt>unihashes</dt>
      <dd>{humanCount(stats.hashequiv_unihashes)}</dd>
      <dd class="muted">
        {humanCount(stats.hashequiv_taskhashes)} taskhashes · {humanCount(
          stats.hashequiv_outhashes,
        )} outhashes
      </dd>
    </div>
    <div>
      <dt>requests</dt>
      {#if requests}
        <dd>{humanCount(requests.num)}</dd>
        <dd class="muted">{humanSeconds(requests.total_time)} total · since restart</dd>
      {:else if requestsError}
        <dd class="muted">n/a</dd>
        <dd class="muted">{requestsError}</dd>
      {:else}
        <dd class="muted">…</dd>
      {/if}
    </div>
    {#if requests}
      <div>
        <dt>latency</dt>
        <dd>{humanSeconds(requests.average)} <span class="label">avg</span></dd>
        <dd class="muted">
          stddev {requests.stdev
            ? '± '
            : ''}{humanSeconds(requests.stdev)} · max {humanSeconds(requests.max_time)}
        </dd>
      </div>
    {/if}
  </dl>
</section>

<style>
  .panel {
    background: var(--panel);
    border: 1px solid var(--panel-border);
    border-radius: 8px;
    padding: 20px;
  }

  header {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: 12px;
    margin-bottom: 8px;
  }

  .grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
    gap: 16px;
    margin: 0;
    padding: 12px 0 0;
    border-top: 1px solid var(--panel-border);
  }

  .grid > div {
    display: flex;
    flex-direction: column;
    gap: 2px;
  }

  dt {
    font-size: 11px;
    text-transform: uppercase;
    letter-spacing: 0.08em;
    color: var(--fg-muted);
  }

  dd {
    margin: 0;
    font-variant-numeric: tabular-nums;
  }

  dd:first-of-type {
    font-size: 20px;
    font-weight: 600;
    color: var(--accent);
  }

  .label {
    font-size: 12px;
    font-weight: 400;
    color: var(--fg-muted);
  }

  dd.muted:first-of-type {
    color: var(--fg-muted);
  }
</style>
