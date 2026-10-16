<script lang="ts">
  /**
   * Auto-ping settings modal: one 3-way mode (Off / On reset / Schedule) plus
   * the anchored schedule form. Port of upstream decolua/9router
   * `AutoPingScheduleModal.js` (be51142b, 28d40159).
   */
  import Button from '../../lib/ui/Button.svelte'
  import Input from '../../lib/ui/Input.svelte'
  import Modal from '../../lib/ui/Modal.svelte'
  import Toggle from '../../lib/ui/Toggle.svelte'
  import {
    getLocalTimezone,
    getScheduleSlotLabels,
    maxSlotsPerDay,
    parseAnchor,
    type NormalizedPingSchedule,
  } from './pingSchedule'

  export type AutoPingMode = 'off' | 'reset' | 'schedule'

  export interface AutoPingSave {
    reactive: boolean
    schedule: NormalizedPingSchedule | { enabled: false }
  }

  interface Props {
    isOpen: boolean
    connectionName?: string
    reactive?: boolean
    schedule?: NormalizedPingSchedule | { enabled?: boolean } | null
    onClose: () => void
    onSave: (save: AutoPingSave) => void
  }

  let { isOpen, connectionName = '', reactive = false, schedule = null, onClose, onSave }: Props =
    $props()

  type FormState = { mode: AutoPingMode; anchor: string; timezone: string; windowHours: string; slotsPerDay: string }

  // Mounted only while open, so the initial state is always fresh.
  function toFormState(
    sched: NormalizedPingSchedule | { enabled?: boolean } | null,
    react: boolean,
  ): FormState {
    return {
      mode: (sched as NormalizedPingSchedule)?.enabled === true ? 'schedule' : react ? 'reset' : 'off',
      anchor: (sched as NormalizedPingSchedule)?.anchor ?? '09:30',
      timezone: getLocalTimezone(),
      windowHours: String((sched as NormalizedPingSchedule)?.windowHours ?? 5),
      slotsPerDay: String((sched as NormalizedPingSchedule)?.slotsPerDay ?? 3),
    }
  }

  let form = $state<FormState>(toFormState(schedule, reactive))

  let windowHours = $derived(Number(form.windowHours))
  let slotsPerDay = $derived(Number(form.slotsPerDay))
  let anchorValid = $derived(Boolean(parseAnchor(form.anchor)))
  let windowValid = $derived(Number.isFinite(windowHours) && windowHours >= 0.5 && windowHours <= 24)
  let slotsMax = $derived(windowValid ? maxSlotsPerDay(windowHours) : 24)
  let slotsValid = $derived(Number.isInteger(slotsPerDay) && slotsPerDay >= 1 && slotsPerDay <= slotsMax)
  let canSaveSchedule = $derived(anchorValid && windowValid && slotsValid)
  let canSave = $derived(form.mode !== 'schedule' || canSaveSchedule)

  let previewSlots = $derived(
    canSaveSchedule
      ? getScheduleSlotLabels({ enabled: true, ...form, windowHours, slotsPerDay })
      : [],
  )

  // Last slot's window end — the moment coverage stops until the next anchor.
  let coverageEnd = $derived.by(() => {
    if (!previewSlots.length || !windowValid) return ''
    const parsed = parseAnchor(form.anchor)
    if (!parsed) return ''
    const total = Math.round(parsed.hour * 60 + parsed.minute + slotsPerDay * windowHours * 60) % 1440
    return `${String(Math.floor(total / 60)).padStart(2, '0')}:${String(total % 60).padStart(2, '0')}`
  })

  const MODES: Array<{ value: AutoPingMode; label: string; icon: string }> = [
    { value: 'off', label: 'Off', icon: 'block' },
    { value: 'reset', label: 'On reset', icon: 'bolt' },
    { value: 'schedule', label: 'Schedule', icon: 'schedule' },
  ]

  function handleSave(): void {
    if (!canSave) return
    onSave({
      reactive: form.mode === 'reset',
      schedule:
        form.mode === 'schedule'
          ? { enabled: true, anchor: form.anchor, timezone: form.timezone, windowHours, slotsPerDay }
          : { enabled: false },
    })
    onClose()
  }
</script>

<Modal {isOpen} {onClose} title="Auto-ping settings" size="lg">
  <div class="flex flex-col gap-4">
    <p class="text-sm text-text-muted">
      Ping at fixed local times so the 5h windows line up with the hours you actually work. The
      overnight gap is what pulls the next day back onto the anchor. Works on its own{#if connectionName}
        for {connectionName}{/if}, and runs alongside auto-ping when that is on too.
    </p>

    <!-- 3-way mode toggle -->
    <div class="grid grid-cols-3 gap-1 rounded-[10px] bg-surface-2 p-1" role="radiogroup" aria-label="Auto-ping mode">
      {#each MODES as m (m.value)}
        <button
          type="button"
          role="radio"
          aria-checked={form.mode === m.value}
          onclick={() => (form = { ...form, mode: m.value })}
          class="flex items-center justify-center gap-1.5 rounded-lg px-2 py-2 text-xs font-medium transition-colors cursor-pointer {form.mode === m.value
            ? 'bg-surface text-text-main shadow-sm'
            : 'text-text-muted hover:text-text-main'}"
        >
          <span class="material-symbols-outlined text-[16px]">{m.icon}</span>
          {m.label}
        </button>
      {/each}
    </div>

    {#if form.mode === 'schedule'}
      <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
        <Input
          label="Anchor time"
          type="time"
          bind:value={form.anchor}
          error={anchorValid ? '' : 'Use HH:MM'}
        />
        <div class="flex flex-col gap-1.5">
          <span class="text-sm font-medium text-text-main">Timezone</span>
          <span class="w-full py-2.5 px-3 text-sm text-text-main bg-surface-2 rounded-[10px]">
            {form.timezone} (local)
          </span>
        </div>
        <Input
          label="Window length (hours)"
          type="number"
          min="0.5"
          max="24"
          bind:value={form.windowHours}
          error={windowValid ? '' : '0.5 – 24'}
        />
        <Input
          label="Pings per day"
          type="number"
          min="1"
          max={String(slotsMax)}
          bind:value={form.slotsPerDay}
          error={slotsValid ? '' : `1 – ${slotsMax}`}
          hint={slotsValid ? `Max ${slotsMax} without overlapping the next anchor` : ''}
        />
      </div>

      {#if previewSlots.length > 0}
        <div class="rounded-[10px] bg-surface-2 px-3 py-2.5 text-xs text-text-muted">
          <span class="font-medium text-text-main">{previewSlots.join(' → ')}</span>
          {#if coverageEnd}
            <span> · coverage ends {coverageEnd}</span>
          {/if}
          <span> ({form.timezone})</span>
        </div>
      {/if}
    {/if}

    {#if form.mode === 'reset'}
      <div class="flex items-center justify-between rounded-[10px] bg-surface-2 px-3 py-2.5">
        <span class="text-sm text-text-muted">Sends a tiny request the moment the quota resets.</span>
        <Toggle checked={true} disabled label="On reset enabled" />
      </div>
    {/if}
  </div>

  {#snippet footer()}
    <Button variant="secondary" onclick={onClose}>Cancel</Button>
    <Button onclick={handleSave} disabled={!canSave}>Save</Button>
  {/snippet}
</Modal>
