// Working-directory suggestions for the New Window dialog (the operator, 2026-07-31:
// "the user should definitely not need to type in the full working
// directory"). The corpus is the thread catalog window the browser already
// holds — every directory the operator has actually worked in — so there is
// no server call and no filesystem browsing: the places you go are the places
// you've been.
//
// Ranking is frequency first, recency second: the daily repos appear across
// dozens of threads while a one-off probe directory appears once, so count
// naturally sinks the junk without any denylist. Typing filters by substring
// of the full path; free text remains valid for a brand-new directory.
import { compactPath } from './session_bar.js'

export function cwdSuggestions({ dataset = [], sessions = [], query = '', limit = 8 } = {}) {
  const seen = new Map() // path → { n, at }
  const note = (path, at) => {
    const value = (path ?? '').trim()
    if (!value) return
    const entry = seen.get(value) ?? { n: 0, at: '' }
    entry.n += 1
    if ((at ?? '') > entry.at) entry.at = at ?? ''
    seen.set(value, entry)
  }
  for (const row of dataset) {
    const cell = row?.metadata?.cwd
    if (cell?.state === 'known') note(cell.value, row.updatedAt)
  }
  // Live sessions count too — a directory in use right now belongs in the
  // list even before its thread is indexed.
  for (const s of sessions) note(s?.cwd, s?.createdAt)

  const q = query.trim().toLowerCase()
  return [...seen.entries()]
    .filter(([path]) => !q || path.toLowerCase().includes(q))
    .sort(([, a], [, b]) => b.n - a.n || (b.at > a.at ? 1 : b.at < a.at ? -1 : 0))
    .slice(0, limit)
    .map(([path]) => ({ path, label: compactPath(path) }))
}
