
import { fmtTurnDuration } from './duration_format.js'

const FULL_PATH_SEGMENTS = 3

export function compactPath(path) {
  const raw = (path ?? '').trim()
  if (!raw) return ''
  const tilde = raw.replace(/^\/(?:Users|home)\/[^/]+/, '~').replace(/\/+$/, '')
  const segments = tilde.split('/').filter(Boolean)
  if (segments.length <= FULL_PATH_SEGMENTS) return tilde
  return `…/${segments.slice(-2).join('/')}`
}

// The control is named Priority and uses Standard/Priority choices, but the
// compact bottom bar uses Codex's shorter status vocabulary: fast/default.
export function tierLabel(value) {
  const v = (value ?? '').trim()
  if (!v) return ''
  if (v === 'priority' || v === 'fast') return 'fast'
  if (v === 'standard' || v === 'default') return 'default'
  return v
}

function field(key, text, title) {
  const value = (text ?? '').trim()
  if (!value) return null
  return title && title !== value ? { key, text: value, title } : { key, text: value }
}

// A catalog metadata cell speaks the three-way known/unknown/unsupported
// contract; only a known value has anything the bar can state.
function knownValue(cell) {
  return cell?.state === 'known' ? String(cell.value ?? '') : ''
}

// The read-only pane's bar: the same fields, sourced from the thread's
// canonical catalog row instead of a live session. Everything here is RECORDED
// truth — the branch the thread ran on, the last model and effort it was seen
// with — not an observation of the repo or a promise about a next turn (the operator,
// 2026-07-31: recorded values; the live bar takes over on resume).
export function historicalBarFields(row, took = null) {
  if (!row) return []
  const md = row.metadata ?? {}
  const fields = sessionBarFields({
    provider: row.provider,
    cwd: knownValue(md.cwd),
    gitBranch: knownValue(md.historicalBranch),
    threadModel: knownValue(md.model),
    threadEffort: knownValue(md.effort),
    serviceTier: knownValue(md.priority),
    threadId: row.nativeId,
    threadPending: false,
  })
  const spent = tookField(took)
  // Ahead of the thread id, which closes the bar as the identity register.
  return spent ? [...fields.slice(0, -1), spent, ...fields.slice(-1)] : fields
}

// "12m 40s · 9 turns" — the summed native durations and the number of turns
// that sum actually covers. The count is not decoration: partial coverage is
// the common case on both providers (many turns carry no duration at all), so a
// bare "12m 40s" would read as the whole thread's work when it is only part of
// it. A thread with no coverage gets no field, never a fabricated zero.
export function tookField(took) {
  const label = fmtTurnDuration(took?.durationMs)
  const turns = Number(took?.turns ?? 0)
  if (!label || turns < 1) return null
  return {
    key: 'took',
    text: `${label} · ${turns} turn${turns === 1 ? '' : 's'}`,
    title: `Summed duration of the ${turns} turn${turns === 1 ? '' : 's'} that recorded one — not wall-clock time`,
  }
}

export function sessionBarFields({
  provider,
  cwd,
  gitBranch,
  model,
  threadModel,
  effort,
  threadEffort,
  serviceTier,
  serviceTierConfirmed = false,
  pendingServiceTier,
  threadId,
  threadPending = false,
} = {}) {
  return [
    field('cwd', compactPath(cwd), (cwd ?? '').trim()),
    field('branch', gitBranch),
    // The override if one is set, the thread's own otherwise: the value this
    // session's next turn will actually use, which is also what the menu's
    // selects edit. Same rule for model and effort.
    field('model', (model ?? '').trim() || threadModel),
    field('effort', (effort ?? '').trim() || threadEffort),
    // Service tier is a codex concept; claude has nothing behind the word, so
    // the field never renders there no matter what was passed in.
    provider === 'codex' ? field('tier', [
      (tierLabel(serviceTier) || (pendingServiceTier ? 'unknown' : '')) + (serviceTier && serviceTierConfirmed ? ' ✓' : ''),
      pendingServiceTier ? `${tierLabel(pendingServiceTier)} next turn` : '',
    ].filter(Boolean).join(' → '), [
      serviceTierConfirmed ? '✓ Provider-confirmed speed' : 'Speed has no live provider confirmation',
      pendingServiceTier ? 'Requested for the next turn; awaiting provider confirmation' : '',
    ].filter(Boolean).join('. ')) : null,
    field(
      'thread',
      (threadId ?? '').trim() || (threadPending ? 'native id pending…' : ''),
      threadPending && !threadId ? 'Waiting for the provider-native resumable thread id' : undefined,
    ),
  ].filter(Boolean)
}
