/**
 * Model search filtering with support for:
 * 1. Case-insensitive substring matching
 * 2. Negation / exclusion tokens (!keyword or -keyword)
 * 3. Combined queries (e.g. "claude !haiku")
 * 4. Regular expressions (/pattern/flags or /pattern/)
 *
 * Port of upstream decolua/9router `src/shared/utils/modelSearchFilter.js` (da1756a8).
 */

export interface ParsedSearchQuery {
  raw: string
  isEmpty: boolean
  isRegex: boolean
  regex: RegExp | null
  positiveTerms: string[]
  negativeTerms: string[]
}

const EMPTY_QUERY: ParsedSearchQuery = {
  raw: '',
  isEmpty: true,
  isRegex: false,
  regex: null,
  positiveTerms: [],
  negativeTerms: [],
}

/** Parse a raw search query into a structured query object. */
export function parseSearchQuery(rawQuery: unknown): ParsedSearchQuery {
  if (!rawQuery || typeof rawQuery !== 'string') {
    return { ...EMPTY_QUERY, positiveTerms: [], negativeTerms: [] }
  }
  const trimmed = rawQuery.trim()
  if (!trimmed) {
    return { ...EMPTY_QUERY, positiveTerms: [], negativeTerms: [] }
  }

  // Regex syntax: /pattern/flags or /pattern/
  if (trimmed.startsWith('/')) {
    const fullRegexMatch = trimmed.match(/^\/(.+)\/([a-z]*)$/i)
    if (fullRegexMatch) {
      try {
        const flags = fullRegexMatch[2] || 'i'
        return {
          raw: trimmed,
          isEmpty: false,
          isRegex: true,
          regex: new RegExp(fullRegexMatch[1], flags),
          positiveTerms: [],
          negativeTerms: [],
        }
      } catch {
        // Invalid regex flags or syntax - fallback to literal
      }
    } else if (trimmed.length > 1) {
      // Allow live typing for leading slash, e.g. /claude.*
      try {
        return {
          raw: trimmed,
          isEmpty: false,
          isRegex: true,
          regex: new RegExp(trimmed.slice(1), 'i'),
          positiveTerms: [],
          negativeTerms: [],
        }
      } catch {
        // Syntax not yet valid while typing (e.g. unclosed parenthesis)
      }
    }
  }

  // Standard term parsing with negation support
  const tokens = trimmed.split(/\s+/)
  const positiveTerms: string[] = []
  const negativeTerms: string[] = []
  for (const token of tokens) {
    if (!token) continue
    if ((token.startsWith('!') || token.startsWith('-')) && token.length > 1) {
      negativeTerms.push(token.slice(1).toLowerCase())
    } else {
      positiveTerms.push(token.toLowerCase())
    }
  }
  return {
    raw: trimmed,
    isEmpty: positiveTerms.length === 0 && negativeTerms.length === 0,
    isRegex: false,
    regex: null,
    positiveTerms,
    negativeTerms,
  }
}

export type SearchTarget =
  | string
  | string[]
  | { name?: string; id?: string; value?: string; alias?: string; provider?: string }
  | null
  | undefined

function candidateStrings(target: SearchTarget): string[] {
  if (typeof target === 'string') return [target]
  if (Array.isArray(target)) return target.filter((t): t is string => typeof t === 'string')
  if (target && typeof target === 'object') {
    return [target.name, target.id, target.value, target.alias, target.provider].filter(
      (t): t is string => typeof t === 'string',
    )
  }
  return []
}

/** Check if a set of candidate strings matches the parsed query. */
export function matchesSearch(
  target: SearchTarget,
  queryOrParsed: ParsedSearchQuery | string | null | undefined,
): boolean {
  const parsed =
    typeof queryOrParsed === 'string' || !queryOrParsed
      ? parseSearchQuery(queryOrParsed)
      : queryOrParsed
  if (parsed.isEmpty) return true
  const candidates = candidateStrings(target)
  if (candidates.length === 0) return false

  // Regex
  if (parsed.isRegex && parsed.regex) {
    const fullText = candidates.join(' ')
    const hasNegativeAssertion = /\(\?!|\(\?<!/.test(parsed.raw)
    if (hasNegativeAssertion) return parsed.regex.test(fullText)
    return candidates.some((text) => parsed.regex!.test(text)) || parsed.regex.test(fullText)
  }

  const lowerCandidates = candidates.map((c) => c.toLowerCase())

  // Negations: reject if ANY candidate contains ANY negative term
  for (const neg of parsed.negativeTerms) {
    if (lowerCandidates.some((c) => c.includes(neg))) return false
  }
  // Positive terms: ALL must match at least one candidate
  for (const pos of parsed.positiveTerms) {
    if (!lowerCandidates.some((c) => c.includes(pos))) return false
  }
  return true
}

/** Filter an array of model or combo objects by a search query. */
export function filterModelsByQuery<T>(
  items: T[],
  query: string | ParsedSearchQuery | null | undefined,
): T[] {
  if (!Array.isArray(items) || items.length === 0) return []
  const parsed = typeof query === 'string' || !query ? parseSearchQuery(query) : query
  if (parsed.isEmpty) return items
  return items.filter((item) => matchesSearch(item as unknown as SearchTarget, parsed))
}
