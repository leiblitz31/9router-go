// Usage detail panels (Activity Heatmap, Node Health) start collapsed; the
// Settings > Display switch flips that default. Each panel can still be opened
// on its own for the current view, so this decides the starting state only.
// Mirrors lib/compactNumber.ts: a localStorage preference, no server round trip.
const DETAILS_EXPANDED_KEY = 'detailsExpandedByDefault'

export function isDetailsExpandedByDefault(): boolean {
  if (typeof localStorage === 'undefined') return false
  return localStorage.getItem(DETAILS_EXPANDED_KEY) === '1'
}

export function setDetailsExpandedByDefault(on: boolean): void {
  if (typeof localStorage === 'undefined') return
  localStorage.setItem(DETAILS_EXPANDED_KEY, on ? '1' : '0')
}
