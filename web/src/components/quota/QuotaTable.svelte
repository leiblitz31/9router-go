<script lang="ts">
  // Port of Next.js ProviderLimits/QuotaTable.js for the Go dashboard.
  import {
    formatResetTime,
    formatResetTimeDisplay,
    getRemainingPercentage,
    type NormalizedQuota,
  } from './types'

  interface Props {
    quotas?: NormalizedQuota[]
    compact?: boolean
    sortMode?: 'default' | 'remaining-asc' | 'remaining-desc'
    showSortLabel?: boolean
    onHideQuota?: ((quota: NormalizedQuota) => void) | null
  }

  let {
    quotas = [],
    compact = false,
    sortMode = 'default',
    showSortLabel = false,
    onHideQuota = null,
  }: Props = $props()

  const PAGE_SIZE = 10

  let page = $state(1)

  interface RowQuota extends NormalizedQuota {
    index: number
    remaining: number
  }

  let normalizedQuotas = $derived<RowQuota[]>(
    quotas.map((quota, index) => ({
      ...quota,
      index,
      remaining: getRemainingPercentage(quota),
    })),
  )

  let sortedQuotas = $derived.by<RowQuota[]>(() => {
    if (sortMode === 'remaining-asc') {
      return [...normalizedQuotas].sort(
        (a, b) => a.remaining - b.remaining || a.name.localeCompare(b.name),
      )
    }
    if (sortMode === 'remaining-desc') {
      return [...normalizedQuotas].sort(
        (a, b) => b.remaining - a.remaining || a.name.localeCompare(b.name),
      )
    }
    return normalizedQuotas
  })

  let totalPages = $derived(Math.max(1, Math.ceil(sortedQuotas.length / PAGE_SIZE)))

  $effect(() => {
    // Reset to first page when the data or sort changes
    void sortMode
    void quotas
    page = 1
  })

  $effect(() => {
    if (page > totalPages) page = totalPages
  })

  function colorClassesFor(remaining: number) {
    if (remaining > 70) {
      return {
        text: 'text-green-600 dark:text-green-400',
        bg: 'bg-green-500',
        bgLight: 'bg-green-500/10',
        emoji: '🟢',
      }
    }
    if (remaining >= 30) {
      return {
        text: 'text-yellow-600 dark:text-yellow-400',
        bg: 'bg-yellow-500',
        bgLight: 'bg-yellow-500/10',
        emoji: '🟡',
      }
    }
    return {
      text: 'text-red-600 dark:text-red-400',
      bg: 'bg-red-500',
      bgLight: 'bg-red-500/10',
      emoji: '🔴',
    }
  }

  const cellPad = $derived(compact ? 'py-1 px-1.5' : 'py-2 px-3')
  const nameText = $derived(compact ? 'text-[11px]' : 'text-sm')
  const resetPrimary = $derived(compact ? 'text-[11px]' : 'text-sm')
  const resetSecondary = $derived(compact ? 'text-[10px] leading-tight' : 'text-xs')

  function fmtNum(n?: number): string {
    return (n || 0).toLocaleString()
  }
</script>

{#if quotas && quotas.length > 0}
  <div class="space-y-2">
    <div class="flex items-center justify-between gap-2">
      <div class="text-[10px] text-text-muted">
        {sortedQuotas.length} quota{sortedQuotas.length > 1 ? 's' : ''}
      </div>
      {#if showSortLabel}
        <div
          class="rounded-md border border-border-subtle bg-surface-2 px-2 py-1 text-[10px] text-text-muted"
        >
          Sorted by account remaining
        </div>
      {/if}
    </div>

    <div class="space-y-px">
      {#each sortedQuotas.slice((page - 1) * PAGE_SIZE, page * PAGE_SIZE) as quota (
        `${quota.name}-${quota.index}`
      )}
        {@const isUnlimited = quota.unlimited === true}
        {@const isCreditBalance = quota.isCreditBalance === true}
        {@const colors = isCreditBalance
          ? {
              text: 'text-blue-600 dark:text-blue-400',
              bg: 'bg-blue-500',
              bgLight: 'bg-blue-500/10',
              emoji: '💰',
            }
          : colorClassesFor(quota.remaining)}
        {@const countdown = quota.neverExpires ? '-' : formatResetTime(quota.resetAt)}
        {@const seeDetails =
          !quota.neverExpires &&
          !quota.resetAt &&
          typeof quota.detailUrl === 'string' &&
          quota.detailUrl !== ''}
        {@const resetDisplay = quota.neverExpires
          ? 'Never expires'
          : seeDetails
            ? 'See details'
            : formatResetTimeDisplay(quota.resetAt)}
        {@const resetSecondaryText = seeDetails ? (quota.detailUrl as string) : resetDisplay}
        {@const recurring = quota.recurring !== false}
        {@const countdownLabel = recurring ? `in ${countdown}` : `expires in ${countdown}`}

        <div
          class="flex items-center gap-2 border-b border-border-subtle/60 hover:bg-surface-2/60 transition-colors {cellPad}"
        >
          <!-- Name -->
          <div class="flex w-36 min-w-0 items-center gap-1.5">
            <span class="text-[10px] shrink-0">{colors.emoji}</span>
            <span class="{nameText} font-medium text-text-main truncate">
              {quota.name}
            </span>
          </div>

          <!-- Progress + used/total -->
          <div class="min-w-0 flex-1 {compact ? 'space-y-1' : 'space-y-1.5'}">
            {#if !isUnlimited && !isCreditBalance}
              <div
                class="{compact ? 'h-1' : 'h-1.5'} rounded-full overflow-hidden border {colors.bgLight} {quota.remaining ===
                0
                  ? 'border-border-subtle'
                  : 'border-transparent'}"
              >
                <div
                  class="h-full transition-all duration-300 {colors.bg}"
                  style:width="{Math.min(quota.remaining, 100)}%"
                ></div>
              </div>
            {/if}

            <div
              class="flex items-center justify-between gap-1 min-w-0 {compact
                ? 'text-[10px]'
                : 'text-xs'}"
            >
              <span
                class="text-text-muted truncate"
                title={isUnlimited
                  ? `${fmtNum(quota.used)} used · Unlimited`
                  : isCreditBalance
                    ? `Credit balance: ${(quota.total || 0).toFixed(2)} ${quota.currency || ''}`
                    : `${fmtNum(quota.used)} / ${(quota.total || 0) > 0 ? fmtNum(quota.total) : '∞'}${quota.unit ? ` ${quota.unit}` : ''}`}
              >
                {isUnlimited
                  ? `${fmtNum(quota.used)} used · Unlimited`
                  : isCreditBalance
                    ? `Credit: ${(quota.total || 0).toFixed(2)} ${quota.currency || ''}`
                    : `${fmtNum(quota.used)} / ${(quota.total || 0) > 0 ? fmtNum(quota.total) : '∞'}${quota.unit ? ` ${quota.unit}` : ''}`}
              </span>
              <span
                class="font-medium shrink-0 {isUnlimited
                  ? 'text-green-600 dark:text-green-400'
                  : isCreditBalance
                    ? 'text-blue-600 dark:text-blue-400'
                    : colors.text}"
              >
                {isUnlimited ? 'Unlimited' : isCreditBalance ? '' : `${quota.remaining}%`}
              </span>
            </div>
          </div>

          <!-- Reset time -->
          <div class="min-w-0 shrink">
            {#if countdown !== '-' || resetDisplay || seeDetails}
              {#if compact}
                <div
                  class="{resetPrimary} text-text-main font-medium truncate"
                  title={resetSecondaryText || ''}
                >
                  {#if countdown !== '-'}
                    {countdownLabel}
                  {:else if seeDetails}
                    <a
                      href={quota.detailUrl as string}
                      target="_blank"
                      rel="noopener noreferrer"
                      class="{resetPrimary} inline-flex items-center gap-0.5 hover:text-text-primary hover:underline"
                      title={`Open usage details: ${resetSecondaryText}`}
                      aria-label={`Open usage details for ${quota.name}`}
                    >
                      {resetDisplay}
                      <span class="material-symbols-outlined text-[12px]">open_in_new</span>
                    </a>
                  {:else}
                    {resetDisplay}
                  {/if}
                </div>
              {:else}
                <div class="min-w-0 space-y-0.5">
                  {#if countdown !== '-'}
                    <div class="{resetPrimary} text-text-main font-medium truncate">
                      {countdownLabel}
                    </div>
                  {/if}
                  {#if resetDisplay}
                    <div class="{resetSecondary} text-text-muted truncate">
                      {#if seeDetails}
                        <a
                          href={quota.detailUrl as string}
                          target="_blank"
                          rel="noopener noreferrer"
                          class="{resetSecondary} inline-flex items-center gap-0.5 hover:text-text-primary hover:underline"
                          title={`Open usage details: ${resetSecondaryText}`}
                          aria-label={`Open usage details for ${quota.name}`}
                        >
                          {resetDisplay}
                          <span class="material-symbols-outlined text-[12px]">open_in_new</span>
                        </a>
                      {:else}
                        {resetDisplay}
                      {/if}
                    </div>
                  {/if}
                </div>
              {/if}
            {:else}
              <div class="{resetPrimary} text-text-muted italic">N/A</div>
            {/if}
          </div>

          <!-- Hide action -->
          {#if typeof onHideQuota === 'function'}
            <button
              type="button"
              onclick={() => onHideQuota?.(quota)}
              class="inline-flex h-6 w-6 shrink-0 items-center justify-center rounded-md text-text-muted transition-colors hover:bg-surface-3 hover:text-text-main"
              title="Hide this quota row"
              aria-label="Hide quota {quota.name}"
            >
              <span class="material-symbols-outlined text-[15px]">visibility_off</span>
            </button>
          {/if}
        </div>
      {/each}
    </div>

    {#if totalPages > 1}
      <div class="rounded-md border border-border-subtle bg-surface-2 px-2 py-1.5">
        <div class="flex items-center justify-between gap-2 text-[10px] text-text-muted">
          <span>
            Showing {(page - 1) * PAGE_SIZE + 1}-{Math.min(page * PAGE_SIZE, sortedQuotas.length)}
            of {sortedQuotas.length}
          </span>
          <span>
            Page {page} / {totalPages}
          </span>
        </div>
        <div class="mt-1.5 flex items-center justify-end gap-1">
          <button
            type="button"
            onclick={() => (page = Math.max(1, page - 1))}
            disabled={page === 1}
            class="flex h-6 items-center rounded-md border border-border-subtle px-2 text-[10px] text-text-main transition-colors hover:bg-surface-3 disabled:cursor-not-allowed disabled:opacity-40"
          >
            Prev
          </button>
          <button
            type="button"
            onclick={() => (page = Math.min(totalPages, page + 1))}
            disabled={page === totalPages}
            class="flex h-6 items-center rounded-md border border-border-subtle px-2 text-[10px] text-text-main transition-colors hover:bg-surface-3 disabled:cursor-not-allowed disabled:opacity-40"
          >
            Next
          </button>
        </div>
      </div>
    {/if}
  </div>
{/if}
