<script lang="ts">
  import type { Stats } from '../lib/api';
  import { humanBytes, humanCount } from '../lib/format';
  import EChart, { type EChartOption } from '../lib/EChart.svelte';

  type Props = { stats: Stats };
  let { stats }: Props = $props();

  // Hash-equiv rows have no byte figure on the server (they're a SQLite table,
  // not on-disk blobs) — the pie shows the two blob categories, and the
  // hash-equiv panel below reports its row counts instead.
  const pieOption: EChartOption = $derived({
    tooltip: {
      trigger: 'item',
      valueFormatter: (v) => humanBytes(Number(v)),
    },
    legend: { bottom: 0, textStyle: { color: '#e5e9f0' } },
    series: [
      {
        name: 'Storage',
        type: 'pie',
        radius: ['45%', '72%'],
        avoidLabelOverlap: true,
        itemStyle: { borderRadius: 4, borderColor: '#171a21', borderWidth: 2 },
        label: {
          show: true,
          formatter: (p) => `${p.name}\n${humanBytes(Number(p.value))}`,
          color: '#e5e9f0',
        },
        data: [
          { value: stats.downloads_bytes, name: 'downloads' },
          { value: stats.sstate_bytes, name: 'sstate' },
        ],
      },
    ],
  });

  const totalBytes = $derived(stats.downloads_bytes + stats.sstate_bytes);
</script>

<section class="panel">
  <header>
    <h2>Storage</h2>
    <span class="muted">{humanBytes(totalBytes)} across {humanCount(
        stats.downloads_files + stats.sstate_files,
      )} files</span>
  </header>

  <div class="chart-wrap">
    <EChart option={pieOption} height="360px" />
  </div>

  <dl class="grid">
    <div>
      <dt>downloads</dt>
      <dd>{humanBytes(stats.downloads_bytes)}</dd>
      <dd class="muted">{humanCount(stats.downloads_files)} files</dd>
    </div>
    <div>
      <dt>sstate</dt>
      <dd>{humanBytes(stats.sstate_bytes)}</dd>
      <dd class="muted">
        {humanCount(stats.sstate_files)} files · {humanCount(
          stats.sstate_recipes,
        )} recipes
      </dd>
    </div>
    <div>
      <dt>hash-equiv</dt>
      <dd>{humanCount(stats.hashequiv_unihashes)} unihashes</dd>
      <dd class="muted">
        {humanCount(stats.hashequiv_taskhashes)} taskhashes · {humanCount(
          stats.hashequiv_outhashes,
        )} outhashes
      </dd>
    </div>
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

  .chart-wrap {
    margin: 8px 0 16px;
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
</style>
