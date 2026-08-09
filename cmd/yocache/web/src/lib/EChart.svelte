<script lang="ts">
  import { onMount } from 'svelte';
  // Tree-shakeable ECharts: import only from echarts/core + the specific
  // charts, components, and renderers we register with use(). Importing
  // from the top-level 'echarts' package would side-effect-register every
  // chart type and pull the full ~800KB library into the bundle.
  import { init, use, type ECharts, type ComposeOption } from 'echarts/core';
  import { PieChart, type PieSeriesOption } from 'echarts/charts';
  import {
    TooltipComponent,
    type TooltipComponentOption,
    LegendComponent,
    type LegendComponentOption,
  } from 'echarts/components';
  import { CanvasRenderer } from 'echarts/renderers';
  // Self-registering 'dark' theme preset (~3 KB). Not part of echarts/core;
  // without this import, init(el, 'dark', ...) silently falls back to the
  // default light-oriented palette — visible on tooltip hover in particular.
  import 'echarts/theme/dark';

  use([PieChart, TooltipComponent, LegendComponent, CanvasRenderer]);

  // Narrow the option type to just the pieces registered above — better
  // autocomplete, and a compile error if we ever pass e.g. a bar-chart
  // series through this component without registering BarChart first.
  export type EChartOption = ComposeOption<
    PieSeriesOption | TooltipComponentOption | LegendComponentOption
  >;

  type Props = {
    option: EChartOption;
    height?: string;
  };

  let { option, height = '360px' }: Props = $props();

  let el: HTMLDivElement;
  let chart: ECharts | undefined;

  onMount(() => {
    chart = init(el, 'dark', { renderer: 'canvas' });
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
