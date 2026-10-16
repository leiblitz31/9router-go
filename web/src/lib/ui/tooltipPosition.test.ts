// placeTooltip is DOM-dependent by design, so the suite hands it the rects
// and sizes it needs rather than reaching for a real layout engine.
import { describe, expect, it } from 'bun:test'
import { placeTooltip } from './tooltipPosition'

const DESKTOP = { width: 1200, height: 800 }
const PHONE = { width: 375, height: 700 }

function rect(overrides: Partial<{ top: number; bottom: number; left: number; right: number; width: number; height: number }> = {}) {
  return { top: 200, bottom: 220, left: 200, right: 260, width: 60, height: 20, ...overrides }
}

describe('placeTooltip', () => {
  it('centers the bubble above the trigger', () => {
    const p = placeTooltip({ rect: rect(), tip: { width: 120, height: 30 }, viewport: DESKTOP })
    expect(p.left).toBe(230 - 60)
    expect(p.top).toBe(200 - 30 - 6)
    expect(p.flipped).toBe(false)
  })

  it('keeps a bubble near the right edge fully on screen', () => {
    const p = placeTooltip({
      rect: rect({ left: 330, right: 366, width: 36 }),
      tip: { width: 224, height: 30 },
      viewport: PHONE,
    })
    expect(p.left + 224).toBeLessThanOrEqual(PHONE.width - 8)
    expect(p.left).toBeGreaterThanOrEqual(8)
  })

  it('keeps a bubble near the left edge fully on screen', () => {
    const p = placeTooltip({
      rect: rect({ left: 4, right: 40, width: 36 }),
      tip: { width: 224, height: 30 },
      viewport: PHONE,
    })
    expect(p.left).toBe(8)
  })

  // The reported class: an absolute bubble inside an overflow container is
  // clipped, so the bubble portals to <body> and flips below the trigger
  // when the space above is too short.
  it('flips below the trigger when the space above is too short', () => {
    const p = placeTooltip({
      rect: rect({ top: 10, bottom: 30 }),
      tip: { width: 120, height: 30 },
      viewport: DESKTOP,
    })
    expect(p.top).toBe(30 + 6)
    expect(p.flipped).toBe(true)
  })

  it('honours prefer below', () => {
    const p = placeTooltip({
      rect: rect(),
      tip: { width: 120, height: 30 },
      viewport: DESKTOP,
      prefer: 'below',
    })
    expect(p.top).toBe(220 + 6)
    expect(p.flipped).toBe(false)
  })

  it('clamps a bubble the window cannot show from the gutter', () => {
    const p = placeTooltip({
      rect: rect({ left: 100, right: 160, width: 60 }),
      tip: { width: 500, height: 30 },
      viewport: PHONE,
    })
    expect(p.left).toBe(8)
  })
})
