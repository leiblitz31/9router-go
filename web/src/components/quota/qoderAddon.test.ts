import { describe, expect, it } from 'bun:test'
import { parseQuotaData } from './types'

// Upstream b2aaf83b: Qoder emits addOnQuota only when the account holds a
// free-credit promo grant; personal_standard accounts report user as 0/0
// (their grant lives in addOn).
describe('qoder quota normalization', () => {
  it('surfaces the add-on grant as an Add-on row', () => {
    const rows = parseQuotaData('qoder', {
      quotas: {
        user: { used: 0, total: 0, resetAt: '', neverExpires: true },
        organization: { used: 0, total: 0, resetAt: '', neverExpires: true },
        addOn: {
          used: 400,
          total: 2200,
          unit: 'credits',
          resetAt: null,
          neverExpires: false,
          detailUrl: 'https://qoder.com/usage',
        },
      },
    })
    // Zero-total buckets are noise; only the grant survives.
    expect(rows.map((r) => r.name)).toEqual(['Add-on'])
    expect(rows[0].used).toBe(400)
    expect(rows[0].total).toBe(2200)
    expect(rows[0].unit).toBe('credits')
    expect(rows[0].neverExpires).toBe(false)
    expect(rows[0].detailUrl).toBe('https://qoder.com/usage')
  })

  it('skips zero-total buckets on accounts without a grant', () => {
    const rows = parseQuotaData('qoder-cn', {
      quotas: {
        user: { used: 10, total: 100, unit: 'credits', resetAt: '2026-10-01T00:00:00Z' },
        organization: { used: 0, total: 0, resetAt: '' },
      },
    })
    expect(rows.map((r) => r.name)).toEqual(['Personal'])
    expect(rows[0].remaining).toBeUndefined()
    expect(rows[0].unit).toBe('credits')
  })

  it('flags the 9999 sentinel as neverExpires instead of a reset date', () => {
    const rows = parseQuotaData('qoder', {
      quotas: {
        user: { used: 10, total: 100, unit: 'credits', resetAt: '', neverExpires: true },
      },
    })
    expect(rows).toHaveLength(1)
    expect(rows[0].name).toBe('Personal')
    expect(rows[0].neverExpires).toBe(true)
    expect(rows[0].resetAt).toBeNull()
  })
})
