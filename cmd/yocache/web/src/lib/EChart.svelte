<script lang="ts">
  import { onMount } from 'svelte';
  import * as echarts from 'echarts';

  type Props = {
    option: echarts.EChartsOption;
    height?: string;
  };

  let { option, height = '360px' }: Props = $props();

  let el: HTMLDivElement;
  let chart: echarts.ECharts | undefined;

  onMount(() => {
    chart = echarts.init(el, 'dark', { renderer: 'canvas' });
    chart.setOption(option);
    const resize = () => chart?.resize();
    window.addEventListener('resize', resize);
    return () => {
      window.removeEventListener('resize', resize);
      chart?.dispose();
      chart = undefined;
    };
  });

  $effect(() => {
    if (chart) chart.setOption(option, true);
  });
</script>

<div bind:this={el} style="width: 100%; height: {height};"></div>
