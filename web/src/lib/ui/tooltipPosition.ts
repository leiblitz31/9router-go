/**
 * Viewport-relative placement for the portalled {@link Tooltip}.
 *
 * The tooltip panel is `position: fixed` and reparented to `<body>`, so it is
 * placed against the trigger's measured rect rather than `position: absolute`
 * inside the trigger's wrapper. An absolute bubble is clipped by any ancestor
 * with `overflow` set, and these triggers sit inside `overflow-y-auto`
 * connection lists and `overflow-x-auto` tables — the same clipping class as
 * the dropdown panels `menuPosition` already fixes.
 *
 * The bubble centers above the trigger and flips below it when the space
 * above is too short, so a tooltip near the top of a short viewport stays on
 * screen. This module places the bubble; it does not size one.
 */

const GUTTER = 8
const GAP = 6

export interface TooltipTriggerRect {
  top: number
  bottom: number
  left: number
  right: number
  width: number
  height: number
}

export interface TooltipSize {
  width: number
  height: number
}

export interface TooltipViewport {
  width: number
  height: number
}

export interface TooltipPlacement {
  left: number
  top: number
  /** True when the bubble rendered on the opposite side of `prefer`. */
  flipped: boolean
}

/** The current window size, or `null` where there is no window at all. */
function currentViewport(): TooltipViewport | null {
  if (typeof window === 'undefined') return null
  return { width: window.innerWidth, height: window.innerHeight }
}

export interface PlaceTooltipOptions {
  rect: TooltipTriggerRect
  tip: TooltipSize
  viewport?: TooltipViewport | null
  prefer?: 'above' | 'below'
}

/** Where a bubble of `tip` size sits when placed against `rect`. */
export function placeTooltip({
  rect,
  tip,
  viewport = currentViewport(),
  prefer = 'above',
}: PlaceTooltipOptions): TooltipPlacement {
  const vw = viewport?.width ?? 0
  const vh = viewport?.height ?? 0
  const width = Math.min(Math.max(tip.width, 1), Math.max(0, vw - GUTTER * 2))

  const cx = rect.left + rect.width / 2
  const left = Math.min(Math.max(cx - width / 2, GUTTER), Math.max(GUTTER, vw - width - GUTTER))

  const aboveTop = rect.top - tip.height - GAP
  const belowTop = rect.bottom + GAP
  const fitsAbove = aboveTop >= GUTTER
  const fitsBelow = belowTop + tip.height <= vh - GUTTER
  const showAbove = prefer === 'above' ? fitsAbove || !fitsBelow : fitsAbove && !fitsBelow
  const top = Math.min(
    Math.max(showAbove ? aboveTop : belowTop, GUTTER),
    Math.max(GUTTER, vh - tip.height - GUTTER),
  )
  return { left, top, flipped: showAbove !== (prefer === 'above') }
}
