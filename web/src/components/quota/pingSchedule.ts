// Anchored ping schedule: fires pings at fixed local wall-clock slots instead of
// (or on top of) the reactive "ping right after reset" behaviour.
//
// With anchor 09:30, a 5h window and 3 slots/day the schedule is
// 09:30 / 14:30 / 19:30 local time — the overnight gap is what re-aligns the
// next day back onto the 09:30 anchor, since 24h is not a multiple of 5h.
//
// Port of upstream decolua/9router `src/shared/services/quotaPingSchedule.js`
// (be51142b, 28d40159).

export function getLocalTimezone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC'
  } catch {
    return 'UTC'
  }
}

export const PING_SCHEDULE_DEFAULTS = {
  anchor: '09:30',
  timezone: getLocalTimezone(),
  windowHours: 5,
  slotsPerDay: 3,
}

/** Fire a slot that came due while the process was down/busy, but never a stale one. */
export const SLOT_GRACE_MS = 30 * 60 * 1000

const HOUR_MS = 3600000
const DAY_MS = 86400000

export interface PingSchedule {
  enabled?: boolean
  anchor?: string
  timezone?: string
  windowHours?: number
  slotsPerDay?: number
}

export interface NormalizedPingSchedule {
  enabled: true
  anchor: string
  timezone: string
  windowHours: number
  slotsPerDay: number
}

export interface PingSlot {
  key: string
  index: number
  at: number
}

function isValidTimezone(timezone: unknown): boolean {
  if (!timezone || typeof timezone !== 'string') return false
  try {
    new Intl.DateTimeFormat('en-US', { timeZone: timezone })
    return true
  } catch {
    return false
  }
}

export function parseAnchor(anchor: unknown): { hour: number; minute: number } | null {
  const match = /^(\d{1,2}):(\d{2})$/.exec(String(anchor ?? '').trim())
  if (!match) return null
  const hour = Number(match[1])
  const minute = Number(match[2])
  if (hour > 23 || minute > 59) return null
  return { hour, minute }
}

export function maxSlotsPerDay(windowHours: number): number {
  if (!Number.isFinite(windowHours) || windowHours <= 0) return 1
  return Math.max(1, Math.floor(24 / windowHours))
}

function clampNumber(value: unknown, fallback: number, min: number, max: number): number {
  const parsed = typeof value === 'number' ? value : Number(value)
  if (!Number.isFinite(parsed)) return fallback
  return Math.min(max, Math.max(min, parsed))
}

/** Fully-defaulted schedule, or null when absent/disabled/invalid. */
export function normalizePingSchedule(raw: unknown): NormalizedPingSchedule | null {
  if (!raw || (raw as PingSchedule).enabled !== true) return null
  const r = raw as PingSchedule
  const anchorParts = parseAnchor(r.anchor) ?? parseAnchor(PING_SCHEDULE_DEFAULTS.anchor)!
  const anchor = `${String(anchorParts.hour).padStart(2, '0')}:${String(anchorParts.minute).padStart(2, '0')}`
  const timezone = isValidTimezone(r.timezone) ? (r.timezone as string) : PING_SCHEDULE_DEFAULTS.timezone
  const windowHours = clampNumber(r.windowHours, PING_SCHEDULE_DEFAULTS.windowHours, 0.5, 24)
  const slotsPerDay = Math.round(
    clampNumber(r.slotsPerDay, PING_SCHEDULE_DEFAULTS.slotsPerDay, 1, maxSlotsPerDay(windowHours)),
  )
  return { enabled: true, anchor, timezone, windowHours, slotsPerDay }
}

function zonedParts(utcMs: number, timezone: string): Record<string, number> {
  const parts = new Intl.DateTimeFormat('en-US', {
    timeZone: timezone,
    hourCycle: 'h23',
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  }).formatToParts(new Date(utcMs))
  const out: Record<string, number> = {}
  for (const part of parts) {
    if (part.type !== 'literal') out[part.type] = Number(part.value)
  }
  return out
}

function zoneOffsetMs(utcMs: number, timezone: string): number {
  const p = zonedParts(utcMs, timezone)
  return Date.UTC(p.year, p.month - 1, p.day, p.hour, p.minute, p.second) - utcMs
}

/** Wall-clock time in `timezone` → UTC epoch ms (second pass covers DST shifts). */
function zonedWallClockToUtcMs(
  { year, month, day, hour, minute }: { year: number; month: number; day: number; hour: number; minute: number },
  timezone: string,
): number {
  const naive = Date.UTC(year, month - 1, day, hour, minute)
  const offset = zoneOffsetMs(naive, timezone)
  const candidate = naive - offset
  const refinedOffset = zoneOffsetMs(candidate, timezone)
  return refinedOffset === offset ? candidate : naive - refinedOffset
}

function slotKey(anchorDayMs: number, timezone: string, index: number): string {
  const p = zonedParts(anchorDayMs, timezone)
  const date = `${p.year}-${String(p.month).padStart(2, '0')}-${String(p.day).padStart(2, '0')}`
  return `${date}#${index}`
}

/** All slots belonging to the anchor day that contains `anchorDayMs`. */
function slotsForAnchorDay(schedule: NormalizedPingSchedule, anchorDayMs: number): PingSlot[] {
  const { hour, minute } = parseAnchor(schedule.anchor)!
  const p = zonedParts(anchorDayMs, schedule.timezone)
  const firstAt = zonedWallClockToUtcMs(
    { year: p.year, month: p.month, day: p.day, hour, minute },
    schedule.timezone,
  )
  return Array.from({ length: schedule.slotsPerDay }, (_, index) => ({
    key: slotKey(anchorDayMs, schedule.timezone, index),
    index,
    at: firstAt + index * schedule.windowHours * HOUR_MS,
  }))
}

/** Slots from yesterday + today + tomorrow cover everything due or upcoming. */
export function getScheduleSlots(schedule: unknown, nowMs = Date.now()): PingSlot[] {
  const normalized = normalizePingSchedule(schedule)
  if (!normalized) return []
  return [
    ...slotsForAnchorDay(normalized, nowMs - DAY_MS),
    ...slotsForAnchorDay(normalized, nowMs),
    ...slotsForAnchorDay(normalized, nowMs + DAY_MS),
  ].sort((a, b) => a.at - b.at)
}

/** Most recent slot that is due and still inside the grace window. */
export function getDuePingSlot(
  schedule: unknown,
  nowMs = Date.now(),
  graceMs = SLOT_GRACE_MS,
): PingSlot | null {
  const due = getScheduleSlots(schedule, nowMs).filter(
    (slot) => slot.at <= nowMs && nowMs - slot.at <= graceMs,
  )
  return due.length ? due[due.length - 1] : null
}

export function getNextPingSlot(schedule: unknown, nowMs = Date.now()): PingSlot | null {
  return getScheduleSlots(schedule, nowMs).find((slot) => slot.at > nowMs) ?? null
}

/** "09:30 → 14:30 → 19:30" preview for the UI. */
export function getScheduleSlotLabels(schedule: unknown): string[] {
  const normalized = normalizePingSchedule(schedule)
  if (!normalized) return []
  const { hour, minute } = parseAnchor(normalized.anchor)!
  const startMinutes = hour * 60 + minute
  return Array.from({ length: normalized.slotsPerDay }, (_, index) => {
    const total = Math.round(startMinutes + index * normalized.windowHours * 60) % 1440
    return `${String(Math.floor(total / 60)).padStart(2, '0')}:${String(total % 60).padStart(2, '0')}`
  })
}

// --- Merged from upstream pingschedule.ts (2026-10-25 merge) ---
// TS1149 forbids two files differing only in casing, so the upstream modal
// helpers live here now; the lowercase file was removed and its importers
// re-pointed at this module. The shared names (PING_SCHEDULE_DEFAULTS,
// parseAnchor, maxSlotsPerDay) are the fork's — verified identical in behavior
// for valid inputs, with maxSlotsPerDay additionally guarded as upstream does.

export const MIN_WINDOW_HOURS = 0.5
export const MAX_WINDOW_HOURS = 24

export interface ParsedAnchor {
  hour: number
  minute: number
}

/** "HH:MM" for a count of minutes from midnight, wrapping past 24h. */
function clockLabel(totalMinutes: number): string {
  const wrapped = ((Math.round(totalMinutes) % 1440) + 1440) % 1440
  const hour = Math.floor(wrapped / 60)
  const minute = wrapped % 60
  return `${String(hour).padStart(2, '0')}:${String(minute).padStart(2, '0')}`
}

/**
 * The "09:30 → 14:30 → 19:30" preview.
 *
 * Rounds before the modulo, as upstream does (Math.round(startMinutes + index *
 * windowHours * 60) % 1440): a 5.1h window puts the third slot at
 * 917.9999999999998 minutes, which truncates to 917 and rounds to 918 — a minute
 * of drift the preview must not show differently from the server.
 */
export function scheduleSlotLabels(schedule: {
  anchor: string
  windowHours: number
  slotsPerDay: number
}): string[] {
  const anchor = parseAnchor(schedule.anchor) ?? parseAnchor(PING_SCHEDULE_DEFAULTS.anchor)
  if (!anchor) return []
  const start = anchor.hour * 60 + anchor.minute
  const max = maxSlotsPerDay(schedule.windowHours)
  const count = Math.max(1, Math.min(schedule.slotsPerDay, max))
  const labels: string[] = []
  for (let index = 0; index < count; index += 1) {
    labels.push(clockLabel(start + index * schedule.windowHours * 60))
  }
  return labels
}

/**
 * When the last slot's window closes — the moment coverage stops until the next
 * anchor. Empty when there is no usable slot to measure from.
 */
export function coverageEndLabel(schedule: {
  anchor: string
  windowHours: number
  slotsPerDay: number
}): string {
  const labels = scheduleSlotLabels(schedule)
  if (labels.length === 0) return ''
  const anchor = parseAnchor(schedule.anchor) ?? parseAnchor(PING_SCHEDULE_DEFAULTS.anchor)
  if (!anchor) return ''
  const max = maxSlotsPerDay(schedule.windowHours)
  const count = Math.max(1, Math.min(schedule.slotsPerDay, max))
  return clockLabel(anchor.hour * 60 + anchor.minute + count * schedule.windowHours * 60)
}

export interface ScheduleFormErrors {
  anchor: string
  windowHours: string
  slotsPerDay: string
}

/**
 * Field-level validation for the editor. The messages are what the modal shows
 * under each field, so they name the bound rather than saying "invalid".
 */
export function validateScheduleForm(form: {
  anchor: string
  windowHours: number
  slotsPerDay: number
}): ScheduleFormErrors {
  const anchorOk = parseAnchor(form.anchor) !== null
  const windowOk =
    Number.isFinite(form.windowHours) &&
    form.windowHours >= MIN_WINDOW_HOURS &&
    form.windowHours <= MAX_WINDOW_HOURS
  const slotsMax = windowOk ? maxSlotsPerDay(form.windowHours) : 24
  const slotsOk =
    Number.isInteger(form.slotsPerDay) && form.slotsPerDay >= 1 && form.slotsPerDay <= slotsMax
  return {
    anchor: anchorOk ? '' : 'Use HH:MM',
    windowHours: windowOk ? '' : `${MIN_WINDOW_HOURS} – ${MAX_WINDOW_HOURS}`,
    slotsPerDay: slotsOk ? '' : `1 – ${slotsMax}`
  }
}

export function isScheduleFormValid(errors: ScheduleFormErrors): boolean {
  return errors.anchor === '' && errors.windowHours === '' && errors.slotsPerDay === ''
}
