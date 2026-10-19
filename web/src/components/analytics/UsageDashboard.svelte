<script lang="ts">
  // Details-section dashboard (issue #280): lifetime cards, activity heatmap,
  // and node health above the request-details table. Port of
  // techysy/10router's UsageDashboard; every number obeys the unit-abbreviation
  // switch like the Overview cards.
  import { onMount } from 'svelte'
  import { api, type UsageDashboardResponse, type UsageLifetime } from '../../api/client'
  import { fmtTokens } from '../../lib/compactNumber'
  import Card from '../../lib/ui/Card.svelte'
  import ActivityHeatmap from './ActivityHeatmap.svelte'
  import NodeHealthTable from './NodeHealthTable.svelte'
  import { isDetailsExpandedByDefault } from '../../lib/displayPrefs'

  let data = $state<UsageDashboardResponse | null>(null)
  let loading = $state(true)
  let failed = $state(false)

  onMount(async () => {
    try {
      data = await api.getUsageDashboard()
    } catch {
      failed = true
    } finally {
      loading = false
    }
  })

  const lifetime = $derived(data?.lifetime ?? null)

  // Both detail panels start collapsed unless Settings > Display says
  // otherwise; each one still opens on its own for the current view.
  let heatmapOpen = $state(isDetailsExpandedByDefault())
  let healthOpen = $state(isDetailsExpandedByDefault())

  interface CardDef {
    label: string
    value: string
    sub?: string
    valueClass: string
    shrink?: boolean
  }

  const cards = $derived.by((): CardDef[] => {
    const l: UsageLifetime | null = lifetime
    if (!l) return []
    return [
      { label: 'Total Requests', value: l.totalRequests.toLocaleString(), valueClass: 'text-text-main' },
      { label: 'Lifetime Tokens', value: fmtTokens(l.totalTokens), valueClass: 'text-primary' },
      {
        label: 'Peak Day Tokens',
        value: fmtTokens(l.peakTokens),
        sub: l.peakDate || undefined,
        valueClass: 'text-info',
      },
      {
        label: 'Cache Hit Rate',
        value: l.cacheHitRate != null ? `${l.cacheHitRate}%` : '—',
        // The rate reads the same trailing-7-day history as Most Used Model,
        // so it is a window number, not a lifetime one.
        sub: 'Last 7 days, real cache rows only',
        valueClass: 'text-success',
      },
      {
        label: 'Most Used Model',
        value: l.topModel?.model || '—',
        sub: l.topModel
          ? `Last 7 days ${fmtTokens(l.topModel.tokens, 'en', true)}${l.topModel.provider ? ` · ${l.topModel.provider}` : ''}`
          : undefined,
        valueClass: 'text-warning',
        shrink: true,
      },
    ]
  })
</script>

<div class="flex min-w-0 flex-col gap-6">
  {#if loading}
    <Card padding="md">
      <div class="flex items-center justify-center gap-2 py-8 text-text-muted text-sm">
        <span class="material-symbols-outlined animate-spin text-[20px]" aria-hidden="true">progress_activity</span>
        Loading dashboard...
      </div>
    </Card>
  {:else if failed || !data}
    <Card padding="md">
      <p class="py-6 text-center text-sm text-text-muted">Failed to load usage dashboard</p>
    </Card>
  {:else}
    {#if cards.length > 0}
      <div class="grid min-w-0 grid-cols-1 gap-3 sm:grid-cols-2 md:grid-cols-3 lg:grid-cols-5 sm:gap-4">
        {#each cards as c (c.label)}
          <div class="bg-surface border border-border-subtle rounded-[14px] shadow-[var(--shadow-soft)] flex min-w-0 flex-col gap-1 px-4 py-3">
            <span class="truncate text-text-muted text-sm uppercase font-semibold">{c.label}</span>
            <div class="flex h-8 items-center min-w-0">
              <span
                class="whitespace-nowrap font-bold truncate {c.shrink ? 'text-sm' : 'text-2xl'} {c.valueClass}"
                title={c.value}
              >{c.value}</span>
            </div>
            {#if c.sub}
              <span class="truncate text-[10px] text-text-muted">{c.sub}</span>
            {:else}
              <span class="text-[10px] invisible select-none leading-none">&nbsp;</span>
            {/if}
          </div>
        {/each}
      </div>
    {/if}

    <Card padding="md" title="Activity Heatmap" icon={calIcon} action={heatmapToggle}>
      {#if heatmapOpen}
        <ActivityHeatmap daily={data.daily || []} currentStreak={lifetime?.currentStreak ?? 0} />
      {/if}
    </Card>

    <Card padding="md" title="Node Health" icon={dnsIcon} action={healthToggle}>
      {#if healthOpen}
        <p class="mb-2 text-xs text-text-muted">Last 7 days (≥50 real requests)</p>
        <NodeHealthTable nodes={data.nodes || []} models={data.models || []} />
      {/if}
    </Card>
  {/if}
</div>

{#snippet calIcon()}
  <span class="material-symbols-outlined text-[18px]" aria-hidden="true">calendar_month</span>
{/snippet}
{#snippet dnsIcon()}
  <span class="material-symbols-outlined text-[18px]" aria-hidden="true">dns</span>
{/snippet}
{#snippet heatmapToggle()}
  <button
    type="button"
    aria-expanded={heatmapOpen}
    aria-label="Toggle Activity Heatmap"
    onclick={() => (heatmapOpen = !heatmapOpen)}
    class="p-1 rounded-lg text-text-muted hover:text-text-main hover:bg-surface-2 transition-colors cursor-pointer"
  >
    <span
      class="material-symbols-outlined text-[18px] transition-transform duration-200"
      style:transform={heatmapOpen ? 'rotate(180deg)' : 'rotate(0deg)'}
    >
      expand_more
    </span>
  </button>
{/snippet}
{#snippet healthToggle()}
  <button
    type="button"
    aria-expanded={healthOpen}
    aria-label="Toggle Node Health"
    onclick={() => (healthOpen = !healthOpen)}
    class="p-1 rounded-lg text-text-muted hover:text-text-main hover:bg-surface-2 transition-colors cursor-pointer"
  >
    <span
      class="material-symbols-outlined text-[18px] transition-transform duration-200"
      style:transform={healthOpen ? 'rotate(180deg)' : 'rotate(0deg)'}
    >
      expand_more
    </span>
  </button>
{/snippet}
