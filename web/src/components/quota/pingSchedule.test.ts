import { describe, expect, it } from 'bun:test'
import {
  getLocalTimezone,
  getNextPingSlot,
  getScheduleSlotLabels,
  getScheduleSlots,
  getDuePingSlot,
  maxSlotsPerDay,
  normalizePingSchedule,
  parseAnchor,
  SLOT_GRACE_MS,
} from './pingSchedule'

// Port of upstream tests/unit/quota-ping-schedule.test.js (be51142b, 28d40159).
describe('quota ping schedule', () => {
  it('parses HH:MM anchors', () => {
    expect(parseAnchor('09:30')).toEqual({ hour: 9, minute: 30 })
    expect(parseAnchor('9:30')).toEqual({ hour: 9, minute: 30 })
    expect(parseAnchor('24:00')).toBeNull()
    expect(parseAnchor('09:60')).toBeNull()
    expect(parseAnchor('nope')).toBeNull()
  })

  it('caps slots per day so slots never overlap the next anchor', () => {
    expect(maxSlotsPerDay(5)).toBe(4)
    expect(maxSlotsPerDay(24)).toBe(1)
    expect(maxSlotsPerDay(0.5)).toBe(48)
  })

  it('rejects absent/disabled schedules', () => {
    expect(normalizePingSchedule(null)).toBeNull()
    expect(normalizePingSchedule({ enabled: false })).toBeNull()
    expect(normalizePingSchedule({})).toBeNull()
  })

  it('defaults anchor/timezone/window/slots', () => {
    const n = normalizePingSchedule({ enabled: true })
    expect(n).not.toBeNull()
    expect(n!.anchor).toBe('09:30')
    expect(n!.timezone).toBe(getLocalTimezone())
    expect(n!.windowHours).toBe(5)
    expect(n!.slotsPerDay).toBe(3)
  })

  it('clamps slots to the window max', () => {
    const n = normalizePingSchedule({ enabled: true, windowHours: 5, slotsPerDay: 99 })
    expect(n!.slotsPerDay).toBe(4)
  })

  it('emits fixed local slot labels', () => {
    expect(getScheduleSlotLabels({ enabled: true, anchor: '09:30', windowHours: 5, slotsPerDay: 3 })).toEqual([
      '09:30',
      '14:30',
      '19:30',
    ])
  })

  it('spaces slots by windowHours and finds due/next slots', () => {
    const schedule = { enabled: true, anchor: '09:30', timezone: 'UTC', windowHours: 5, slotsPerDay: 3 }
    // Mid-second-slot: 14:30 UTC + 10 min
    const now = Date.UTC(2026, 9, 15, 14, 40, 0)
    const due = getDuePingSlot(schedule, now)
    expect(due).not.toBeNull()
    expect(due!.at).toBe(Date.UTC(2026, 9, 15, 14, 30, 0))
    const next = getNextPingSlot(schedule, now)
    expect(next!.at).toBe(Date.UTC(2026, 9, 15, 19, 30, 0))
    // Stale slots (older than grace) are never due
    expect(getDuePingSlot(schedule, now + SLOT_GRACE_MS + 60_000)).toBeNull()
    // Empty for disabled
    expect(getScheduleSlots({ enabled: false }, now)).toEqual([])
  })
})
