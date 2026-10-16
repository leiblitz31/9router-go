import { describe, expect, it } from 'bun:test'
import { filterModelsByQuery, matchesSearch, parseSearchQuery } from './modelSearchFilter'

// Port of upstream tests/unit/model-search-filter.test.js (da1756a8).
describe('model search filter', () => {
  it('parses empty queries', () => {
    expect(parseSearchQuery('').isEmpty).toBe(true)
    expect(parseSearchQuery('   ').isEmpty).toBe(true)
    expect(parseSearchQuery(null).isEmpty).toBe(true)
  })

  it('splits positive and negative terms', () => {
    const q = parseSearchQuery('claude !haiku -opus')
    expect(q.positiveTerms).toEqual(['claude'])
    expect(q.negativeTerms).toEqual(['haiku', 'opus'])
  })

  it('matches case-insensitive substrings with exclusions', () => {
    expect(matchesSearch('Claude-3-Opus', 'claude')).toBe(true)
    expect(matchesSearch('Claude-3-Haiku', 'claude !haiku')).toBe(false)
    expect(matchesSearch('GPT-4', 'claude')).toBe(false)
    expect(matchesSearch({ id: 'x', name: 'Claude Haiku', provider: 'anthropic' }, '!haiku')).toBe(
      false,
    )
  })

  it('supports /regex/ search including live-typed leading slash', () => {
    expect(matchesSearch('claude-3-opus', '/opus$/')).toBe(true)
    expect(matchesSearch('claude-3-opus', '/opus')).toBe(true)
    expect(matchesSearch('gpt-4', '/opus/')).toBe(false)
    // Invalid regex falls back to literal terms, never throws
    expect(matchesSearch('a', '/(((/')).toBe(false)
  })

  it('requires ALL positive terms, rejects on ANY negative term', () => {
    expect(matchesSearch('claude opus 200k', 'claude opus')).toBe(true)
    expect(matchesSearch('claude opus', 'claude opus 200k')).toBe(false)
    expect(filterModelsByQuery(['claude-opus', 'claude-haiku', 'gpt-4'], 'claude !haiku')).toEqual([
      'claude-opus',
    ])
    expect(filterModelsByQuery(['a', 'b'], '')).toEqual(['a', 'b'])
  })
})
