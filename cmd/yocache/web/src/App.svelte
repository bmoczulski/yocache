<script lang="ts">
  import { onMount } from 'svelte';
  import { fetchStats, fetchVersion, type Stats, type VersionInfo } from './lib/api';
  import StoragePanel from './panels/StoragePanel.svelte';

  let stats = $state<Stats | null>(null);
  let version = $state<VersionInfo | null>(null);
  let error = $state<string | null>(null);

  onMount(async () => {
    try {
      const [s, v] = await Promise.all([fetchStats(), fetchVersion()]);
      stats = s;
      version = v;
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
    }
  });
</script>

<header class="topbar">
  <h1>YoCache</h1>
  {#if version}
    <span class="muted">
      {version.version}{version.modified ? ' (modified)' : ''}
    </span>
  {/if}
</header>

<main>
  {#if error}
    <p class="error">Failed to load: {error}</p>
  {:else if stats}
    <StoragePanel {stats} />
  {:else}
    <p class="muted">Loading…</p>
  {/if}
</main>

<style>
  .topbar {
    display: flex;
    align-items: baseline;
    gap: 12px;
    padding: 20px 24px;
    border-bottom: 1px solid var(--panel-border);
  }

  h1 {
    font-size: 20px;
    letter-spacing: -0.02em;
  }

  main {
    max-width: 1200px;
    margin: 0 auto;
    padding: 24px;
    display: flex;
    flex-direction: column;
    gap: 20px;
  }
</style>
