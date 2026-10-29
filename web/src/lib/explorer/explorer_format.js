import { runtimeLabel, runtimeReady } from '../shared/lifecycle_state.js'
// Presentation helpers for catalog rows and detail. Pure functions only, so the
// display-title fallback chain and the known/unknown/unsupported metadata mapping
// (a contract requirement) can be tested against the real fixture without Svelte.

import { fmtDuration } from '../shared/duration_format.js'
import { threadWorkIsActive } from '../../explorer_local.js'

const DAY = 86_400_000
const NATIVE_ID_PREFIX = 8
const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec']

// Display name resolution for history headings: a
// user-assigned name wins over the derived title, then preview, then the
// shortened nativeId. kind lets callers style the fallbacks.
export function displayTitle(item) {
  if (item.name?.value) return { text: item.name.value, kind: 'name' }
  if (item.title) return { text: item.title, kind: 'title' }
  if (item.preview) return { text: item.preview, kind: 'preview' }
  return { text: shortNativeId(item.nativeId), kind: 'id' }
}

// Last-resort label for a thread with no name, title, preview or project: the
// head of its native id. Shared with the sidebar projection's title chain.
export function shortNativeId(nativeId) {
  return (nativeId ?? '').slice(0, NATIVE_ID_PREFIX)
}

// The thread's actual user-assigned name, or null. Rename affordances edit
// this — never a fallback label.
export function displayName(item) {
  return item.name?.value ?? null
}

// One label chain for a thread row, shared verbatim by the compact rail and
// the expanded Explorer list. Rule (the operator, 2026-07-29): the Explorer is an
// expansion of the compact view — the two surfaces must never disagree.
// An unnamed, untitled thread renders the muted dash in both: the working
// directory is already displayed on every row (rail second line, table
// project column), so repeating it as a title stand-in is noise (the operator, same
// conversation).
export function rowLabel({ name, title, preview, kind }) {
  if (name) return { text: name, kind: 'name' }
  if (title) return { text: title, kind: 'title' }
  // A worker's first prompt is its brief: without it every unnamed subagent
  // collapses to an indistinguishable dash in the sidebar.
  if (kind === 'subagent' && preview) return { text: preview, kind: 'preview' }
  return { text: '–', kind: 'none' }
}

// Names and derived titles describe the thread; the 'none' placeholder gets
// muted — by BOTH surfaces, through this one predicate.
export function labelIsFallback(kind) {
  return kind === 'none'
}

export function cwdTail(path) {
  if (!path) return null
  return path.split('/').filter(Boolean).at(-1) ?? path
}

// Column timestamp in the browser's local time, split into {day, time} parts so
// the UI can align the time across rows regardless of day-label width:
// {today, 2:32 PM} / {yesterday, 9:15 AM} / {Jul 8, 2:32 PM} within the current
// year / {Jul 8, 2025} for older years (year sits in the time slot). Null when
// missing/unparseable (rendered as a muted placeholder — never fabricated).
export function neatTime(iso, now = new Date()) {
  if (!iso) return null
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return null
  const time = clockTime(d)
  const midnight = (x) => new Date(x.getFullYear(), x.getMonth(), x.getDate()).getTime()
  const daysAgo = Math.round((midnight(now) - midnight(d)) / DAY)
  if (daysAgo === 0) return { day: 'today', time }
  if (daysAgo === 1) return { day: 'yesterday', time }
  const monthDay = `${MONTHS[d.getMonth()]} ${d.getDate()}`
  return d.getFullYear() === now.getFullYear()
    ? { day: monthDay, time }
    : { day: monthDay, time: String(d.getFullYear()) }
}

// 12-hour local clock time: "2:32 PM". Single-digit hours get a no-break
// space so every time is the same width and digits align in the mono,
// right-aligned timestamp columns.
function clockTime(d) {
  const h = d.getHours()
  const h12 = h % 12 === 0 ? 12 : h % 12
  const pad = h12 < 10 ? '\u00A0' : ''
  return `${pad}${h12}:${String(d.getMinutes()).padStart(2, '0')} ${h < 12 ? 'AM' : 'PM'}`
}

// Three-way render mode for one canonical metadata field. This distinction is a
// contract requirement: unknown and unsupported must not look like a known value.
export function metaRenderModel(field, provider) {
  const state = field?.state ?? 'unknown'
  if (state === 'known') return { mode: 'known', value: field.value, evidence: field.evidence }
  if (state === 'unsupported') return { mode: 'unsupported', provider }
  return { mode: 'unknown' }
}

// Per-column metadata cells for the grid list: only KNOWN metadata carries a
// value; unknown/unsupported cells are null (rendered as a muted placeholder,
// never a fabricated value). The detail pane shows the full three-way states.
// Display name for a model id: the vendor prefix is noise in a column that
// sits next to the provider column, so "claude-fable-5" reads "fable-5".
// The verbatim id stays in the row tooltip and the history view.
export function displayModel(model) {
  if (!model) return null
  return model.startsWith('claude-') ? model.slice('claude-'.length) : model
}

export function sidebarAge(iso, now = Date.now()) {
  const at = new Date(iso ?? '').getTime()
  if (!Number.isFinite(at) || at <= 0) return ''
  const seconds = Math.floor((now - at) / 1000)
  if (seconds < 0) return ''
  if (seconds < 15) return 'now'
  if (seconds < 60) return '<1m'
  if (seconds < 3_600) return `${Math.floor(seconds / 60)}m`
  if (seconds < 86_400) return `${Math.floor(seconds / 3_600)}h`
  return `${Math.floor(seconds / 86_400)}d`
}

export function sidebarAgeClass(iso, now = Date.now()) {
  const at = new Date(iso ?? '').getTime()
  if (!Number.isFinite(at) || at <= 0 || now < at) return ''
  const elapsed = now - at
  if (elapsed < 3_600_000) return 'fresh'
  if (elapsed < 86_400_000) return 'warm'
  return 'stale'
}

export function rowCells(item) {
  const m = item.metadata ?? {}
  const known = (f) => (f?.state === 'known' && f.value ? f.value : null)
  const cwd = known(m.cwd)
  const branch = known(m.historicalBranch)
  const model = known(m.model)
  return {
    project: cwd ? { text: cwdTail(cwd), title: cwd } : null,
    model: model ? { text: displayModel(model), title: model } : null,
    effort: known(m.effort),
    branch: branch ? `⎇ ${branch}` : null,
  }
}

// Activity dot from the server-owned runtime overlay. One indicator slot with
// working, idle, and detached states, rendered
// identically by the sidebar (.t-dot) and the Explorer's dot column (.xdot);
// the title carries the specifics. Null when the thread is live nowhere and
// nothing remembers it.
const DETACHED_TITLE = "Detached: nano's session ended — click to open history"

// A turn parked on a dialog is not working (D15): the agent has stopped and is
// waiting on the operator, so it gets its own dot state rather than sharing the
// working one with a ticking clock beside it. Both providers keep the turn
// active while a request is parked, so this reads off `waiting`, not off a
// suspended turn state.
function nanoDotState(runtime) {
  return runtimeLabel(runtime)
}

const NANO_DOT_TITLES = {
  working: 'nano session (working)',
  waiting: 'nano session (waiting for you)',
  idle: 'nano session',
  starting: 'nano session (starting)',
  resting: 'nano session (resting since a restart; reconnects when needed)',
  reconnecting: 'nano session (reconnecting)',
  parked: 'nano session (reconnect parked)',
  unreachable: 'nano session (unreachable)',
  detached: 'nano session (detached)',
  exited: 'nano session (exited)',
  unknown: 'nano session (connection unknown)',
}

// One compact roll-up for both catalog surfaces. Current descendants are the
// group the caret reveals; historical descendants remain a secondary lazy
// action; working is a row-local subset of the current group.
export function workerSummaryText(counts) {
  const current = Number(counts?.current ?? 0)
  const historical = Number(counts?.historical ?? 0)
  const working = Number(counts?.working ?? 0)
  if (current + historical === 0) return null
  return `subagents: ${current} (${historical}) · ${working} working`
}

export function activityDot(item) {
  const runtime = item.runtime
  // After a restart a stored thread is starting before its session exists, or
  // resting until something needs it: Nano brings it back by itself, so it is
  // not shown as detached.
  const hosted = runtime?.attached || runtime?.status === 'starting' || runtime?.status === 'resting'
  const live = []
  if (hosted) live.push(NANO_DOT_TITLES[nanoDotState(runtime)])
  if (item.kind === 'subagent' && threadWorkIsActive(item)) live.push('worker running')
  const activeTitle = `Active: ${live.join(' · ')}`
  if (hosted) {
    const state = nanoDotState(runtime)
    return { state, title: activeTitle }
  }
  if (threadWorkIsActive(item)) return { state: 'working', title: activeTitle }
  if (runtime?.detached) {
    const alsoLive = live.length > 0 ? ` · ${activeTitle}` : ''
    return { state: 'detached', title: `${DETACHED_TITLE}${alsoLive}` }
  }
  return null
}

// --- The working elapsed a live row displays -------------------------------
//
// One source for the rail and the expanded Explorer, so the two surfaces can
// never disagree about how long a thread has been working (the rail⇄Explorer
// parity rule, the operator 2026-07-29). They place it differently — the rail reuses its
// right-hand age slot, the table puts a chip on the title line — but the string
// and the decision to show one at all come from here.
//
// Three outcomes, and the third is the one that matters:
//   working + anchored → the ticking elapsed;
//   waiting            → the word, never a clock (D15);
//   working, NO anchor → the state with an EMPTY text. A re-attach that
//                        recovered no evidence renders the dot and nothing
//                        beside it rather than a start nano made up (D2/D8).
export function workingIndicator(item, now = Date.now()) {
  const runtime = item?.runtime
  if (!runtime?.attached || !runtimeReady(runtime)) return null
  if (runtime.threadStatus === 'needs_you') return { state: 'waiting', text: 'waiting', title: 'Waiting for you to answer' }
  if (runtime.threadStatus !== 'working') return null
  const anchor = tickingAnchor(item)
  return {
    state: 'working',
    text: anchor ? fmtDuration(now - anchor) : '',
    title: anchor ? 'Working — elapsed for this turn' : 'Working — start time unknown',
  }
}

// The anchor a timer should tick against, or 0 when nothing should tick. Kept
// separate from the formatting so a component can schedule its own ticks
// without re-rendering to find out whether it needs to (D6: non-working rows
// never wake for the timer).
export function tickingAnchor(item) {
  const runtime = item?.runtime
  if (!runtime?.attached || !runtimeReady(runtime) || runtime.threadStatus !== 'working') return 0
  const anchor = Number(runtime.turnStartedAtMs ?? 0)
  return Number.isFinite(anchor) && anchor > 0 ? anchor : 0
}

// D6: tick to the next visible change, not on one global interval. Under ten
// seconds the display carries a tenth, so it needs 100ms; past that only the
// whole second moves, so the next wake is the next second boundary — which also
// keeps a screenful of rows from drifting out of step with each other.
export function nextTickDelayMs(elapsedMs) {
  if (elapsedMs < 10_000) return 100
  const untilNextSecond = 1000 - (elapsedMs % 1000)
  return untilNextSecond <= 0 ? 1000 : untilNextSecond
}

// Health-column badges reusing the .ifile-badge shapes. Role badges render on
// the shared title line, so native subagents do not duplicate that evidence.
export function healthBadges(item) {
  const src = item.source ?? {}
  const badges = []
  if (item.archived) badges.push({ tone: 'neutral', text: 'archived' })
  if (src.status === 'source_missing') badges.push({ tone: 'missing', text: 'missing' })
  if (src.conflict) badges.push({ tone: 'missing', text: 'conflict' })
  if (src.diagnosticCount > 0) badges.push({ tone: 'diag', text: `${src.diagnosticCount} diag` })
  return badges
}

// Programmatic rows get a short origin badge next to the title, keyed off the
// verbatim origin value so the label can never drift from the backend mapping
// table; a programmatic origin without a short label renders verbatim.
// Null for non-programmatic rows.
const PROGRAMMATIC_BADGE_LABELS = { 'sdk-cli': 'sdk', codex_exec: 'exec' }

export function programmaticBadge(item) {
  if (item.interaction !== 'programmatic') return null
  const origin = item.metadata?.origin?.value
  return PROGRAMMATIC_BADGE_LABELS[origin] ?? origin ?? 'run'
}

// Whole-row render model, assembled from the pieces above. started/updated are
// null when the row has no usable createdAt/updatedAt.
export function rowRenderModel(item, now = new Date()) {
  const m = item.metadata ?? {}
  return {
    provider: item.provider,
    kind: item.kind,
    name: displayName(item),
    label: rowLabel({ name: displayName(item), title: item.title, preview: item.preview, kind: item.kind }),
    started: neatTime(item.createdAt, now),
    updated: neatTime(item.updatedAt, now),
    cells: rowCells(item),
    badges: healthBadges(item),
    dot: activityDot(item),
    programmatic: programmaticBadge(item),
  }
}
