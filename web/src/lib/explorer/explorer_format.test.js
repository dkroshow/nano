import { describe, it, expect } from 'vitest'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { displayModel, displayTitle, labelIsFallback, metaRenderModel, neatTime, rowLabel, rowRenderModel, programmaticBadge, activityDot, sidebarAge, sidebarAgeClass, workerSummaryText, workingIndicator, tickingAnchor, nextTickDelayMs } from './explorer_format.js'
import { projectSidebar } from './sidebar_projection.js'

const fixture = JSON.parse(
  readFileSync(
    fileURLToPath(new URL('../../../../internal/app/testdata/thread_catalog/threads-page.json', import.meta.url)),
    'utf8'
  )
)
const byId = Object.fromEntries(fixture.items.map((it) => [it.id, it]))
const appSource = readFileSync(fileURLToPath(new URL('../../App.svelte', import.meta.url)), 'utf8')

describe('displayTitle fallback chain', () => {
  it('prefers title', () => {
    expect(displayTitle(byId.nth_codex_main)).toEqual({ text: 'Refactor request routing', kind: 'title' })
  })
  it('falls back to preview when title is null', () => {
    expect(displayTitle(byId.nth_claude_main)).toEqual({ text: 'Review the storage migration.', kind: 'preview' })
  })
  it('falls back to a shortened nativeId when title and preview are null', () => {
    expect(displayTitle(byId.nth_codex_conflict)).toEqual({ text: '55555555', kind: 'id' })
  })
})

describe('displayTitle name precedence', () => {
  it('a user-assigned name outranks title, preview, and id', () => {
    expect(
      displayTitle({
        name: { value: 'influence graph classifier', provenance: 'manual', evidence: 'user_assigned' },
        title: 'derived title',
        preview: 'a preview',
        nativeId: 'abcd1234efgh',
      })
    ).toEqual({ text: 'influence graph classifier', kind: 'name' })
  })
  it('a null/absent name overlay falls through to the derived title', () => {
    expect(displayTitle({ name: null, title: 'Real title', nativeId: 'abcd1234' })).toEqual({
      text: 'Real title',
      kind: 'title',
    })
    expect(displayTitle({ title: 'Real title', nativeId: 'abcd1234' })).toEqual({ text: 'Real title', kind: 'title' })
  })
  it('the named fixture row surfaces its name over its derived title', () => {
    // nth_claude_missing carries both a name and a title; the name wins.
    expect(byId.nth_claude_missing.title).toBe('Retained projection')
    expect(displayTitle(byId.nth_claude_missing)).toEqual({ text: 'storage migration debugging', kind: 'name' })
  })
})

describe('metaRenderModel three-way distinction', () => {
  it('known → value + evidence', () => {
    expect(metaRenderModel(byId.nth_codex_main.metadata.effort, 'codex')).toEqual({
      mode: 'known', value: 'high', evidence: 'native_durable',
    })
  })
  it('unknown → unknown mode', () => {
    expect(metaRenderModel(byId.nth_claude_main.metadata.effort, 'claude')).toEqual({ mode: 'unknown' })
  })
  it('unsupported → unsupported mode carrying provider', () => {
    expect(metaRenderModel(byId.nth_claude_main.metadata.priority, 'claude')).toEqual({
      mode: 'unsupported', provider: 'claude',
    })
  })
})

describe('neatTime', () => {
  // Local-time inputs so the today/yesterday/day boundaries are TZ-stable.
  const now = new Date(2026, 6, 9, 18, 0) // Jul 9 2026, 6:00 PM local
  it('same local day → today, 12-hour clock, single-digit hour padded', () => {
    expect(neatTime(new Date(2026, 6, 9, 15, 4).toISOString(), now)).toEqual({ day: 'today', time: '\u00A03:04 PM' })
  })
  it('previous local day → yesterday', () => {
    expect(neatTime(new Date(2026, 6, 8, 23, 30).toISOString(), now)).toEqual({ day: 'yesterday', time: '11:30 PM' })
  })
  it('midnight and noon map to 12, not 0', () => {
    expect(neatTime(new Date(2026, 6, 9, 0, 5).toISOString(), now)).toEqual({ day: 'today', time: '12:05 AM' })
    expect(neatTime(new Date(2026, 6, 9, 12, 0).toISOString(), now)).toEqual({ day: 'today', time: '12:00 PM' })
  })
  it('same year → month day + time', () => {
    expect(neatTime(new Date(2026, 0, 2, 9, 5).toISOString(), now)).toEqual({ day: 'Jan 2', time: '\u00A09:05 AM' })
  })
  it('older year → month day + year in the time slot', () => {
    expect(neatTime(new Date(2025, 11, 31, 9, 5).toISOString(), now)).toEqual({ day: 'Dec 31', time: '2025' })
  })
  it('returns null for missing/unparseable input', () => {
    expect(neatTime(null, now)).toBeNull()
    expect(neatTime('not-a-date', now)).toBeNull()
  })
})

describe('rowLabel — the one chain both list surfaces render (the operator, 2026-07-29)', () => {
  it('chains name → title → muted dash; the cwd has its own line and is never repeated as a title', () => {
    expect(rowLabel({ name: 'graph', title: 'Derived' })).toEqual({ text: 'graph', kind: 'name' })
    expect(rowLabel({ title: 'Derived' })).toEqual({ text: 'Derived', kind: 'title' })
    expect(rowLabel({})).toEqual({ text: '–', kind: 'none' })
  })

  it('marks the dash placeholder as the fallback; names and derived titles are real', () => {
    expect(labelIsFallback('name')).toBe(false)
    expect(labelIsFallback('title')).toBe(false)
    expect(labelIsFallback('none')).toBe(true)
  })

  it('uses a subagent brief when the worker has no assigned or derived title', () => {
    expect(rowLabel({ kind: 'subagent', preview: 'Inspect the parser' })).toEqual({
      text: 'Inspect the parser', kind: 'preview',
    })
  })

  it('compact rail and Explorer row agree on every fixture item — the Explorer is an expansion of the compact view', () => {
    for (const item of fixture.items) {
      const row = projectSidebar([item], [])[0]
      const m = rowRenderModel(item)
      expect(row.title).toBe(m.label.text)
      expect(row.titleKind).toBe(m.label.kind)
    }
  })
})

describe('rowRenderModel over every fixture item', () => {
  const CELL_KEYS = ['project', 'model', 'effort', 'branch']

  it('produces a valid grid render model for each fixture row', () => {
    for (const item of fixture.items) {
      const m = rowRenderModel(item)
      // m.name seeds the rename editor: the user-assigned name or null —
      // never a fallback. What the cell RENDERS is m.label (shared chain).
      expect(m.name === null || typeof m.name === 'string').toBe(true)
      expect(m.name).toBe(item.name?.value ?? null)
      expect(typeof m.label.text).toBe('string')
      expect(typeof m.label.kind).toBe('string')
      // timestamps are {day, time} parts or null — never a fabricated value.
      for (const t of [m.started, m.updated]) {
        expect(t === null || (typeof t.day === 'string' && typeof t.time === 'string')).toBe(true)
      }
      // every grid column cell exists as a key; non-known metadata is null.
      for (const k of CELL_KEYS) expect(k in m.cells).toBe(true)
      expect(Array.isArray(m.badges)).toBe(true)
    }
  })

  it('fully-known codex row fills every metadata cell', () => {
    const m = rowRenderModel(byId.nth_codex_main)
    expect(m.cells).toEqual({
      project: { text: 'nano', title: '/workspace/nano' },
      model: { text: 'gpt-example', title: 'gpt-example' },
      effort: 'high',
      branch: '⎇ main',
    })
    expect(m.badges).toEqual([])
  })

  it('conflict fixture: blank name, null timestamps, conflict + diag badges', () => {
    const m = rowRenderModel(byId.nth_codex_conflict)
    expect(m.name).toBeNull()
    expect(m.started).toBeNull()
    expect(m.updated).toBeNull()
    expect(m.cells).toEqual({ project: null, model: null, effort: null, branch: null })
    expect(m.badges).toEqual([{ tone: 'missing', text: 'conflict' }, { tone: 'diag', text: '1 diag' }])
  })

  it('missing fixture: red missing badge', () => {
    const m = rowRenderModel(byId.nth_claude_missing)
    expect(m.badges).toContainEqual({ tone: 'missing', text: 'missing' })
  })

  it('archived fixture: neutral archived badge; unknown cells stay null', () => {
    const m = rowRenderModel(byId.nth_codex_archived)
    expect(m.badges).toEqual([{ tone: 'neutral', text: 'archived' }])
    expect(m.cells).toEqual({
      project: { text: 'archive', title: '/workspace/archive' },
      model: null, effort: null, branch: null,
    })
  })

  it('subagent fixture: health column omits the duplicate native subagent role badge', () => {
    const m = rowRenderModel(byId.nth_claude_subagent)
    expect(m.badges).toEqual([])
    expect(m.cells.branch).toBe('⎇ main')
  })

  it('programmatic rows carry a short origin badge, others do not', () => {
    // codex codex_exec → exec, claude sdk-cli → sdk, everything else → null.
    expect(rowRenderModel(byId.nth_codex_archived).programmatic).toBe('exec')
    expect(rowRenderModel(byId.nth_claude_missing).programmatic).toBe('sdk')
    expect(rowRenderModel(byId.nth_codex_main).programmatic).toBeNull()
    expect(rowRenderModel(byId.nth_codex_conflict).programmatic).toBeNull()
  })
})

describe('compact subagent summary', () => {
  it('states current, historical, and working counts in one line', () => {
    expect(workerSummaryText({ current: 5, historical: 3, working: 2 })).toBe('subagents: 5 (3) · 2 working')
    expect(workerSummaryText({ current: 1, historical: 0, working: 0 })).toBe('subagents: 1 (0) · 0 working')
    expect(workerSummaryText({ current: 0, historical: 0, working: 0 })).toBeNull()
  })

  it('uses neutral agents wording in both background-disclosure strings', () => {
    expect(appSource).toContain('Background agents')
    expect(appSource).toContain('Background agents are folded.')
    expect(appSource).not.toContain('Background workers')
  })
})

describe('displayModel', () => {
  it('strips the claude vendor prefix and leaves other ids verbatim', () => {
    expect(displayModel('claude-fable-5')).toBe('fable-5')
    expect(displayModel('claude-haiku-4-5-20251001')).toBe('haiku-4-5-20251001')
    expect(displayModel('gpt-5.6-sol')).toBe('gpt-5.6-sol')
    expect(displayModel(null)).toBeNull()
  })

  it('model cell pairs the display name with the verbatim id as tooltip', () => {
    expect(rowRenderModel(byId.nth_claude_main).cells.model).toEqual({
      text: 'example',
      title: 'claude-example',
    })
    expect(rowRenderModel(byId.nth_codex_main).cells.model).toEqual({
      text: 'gpt-example',
      title: 'gpt-example',
    })
    expect(rowRenderModel(byId.nth_codex_archived).cells.model).toBeNull()
  })
})

describe('sidebar recency', () => {
  const now = Date.UTC(2026, 6, 28, 18, 0, 0)
  const at = (offsetMs) => new Date(now - offsetMs).toISOString()

  it('uses standard-compatible compact labels', () => {
    expect(sidebarAge(at(5_000), now)).toBe('now')
    expect(sidebarAge(at(30_000), now)).toBe('<1m')
    expect(sidebarAge(at(12 * 60_000), now)).toBe('12m')
    expect(sidebarAge(at(3 * 3_600_000), now)).toBe('3h')
    expect(sidebarAge(at(2 * 86_400_000), now)).toBe('2d')
  })

  it('uses fresh, warm, and stale color tiers', () => {
    expect(sidebarAgeClass(at(59 * 60_000), now)).toBe('fresh')
    expect(sidebarAgeClass(at(2 * 3_600_000), now)).toBe('warm')
    expect(sidebarAgeClass(at(2 * 86_400_000), now)).toBe('stale')
    expect(sidebarAgeClass(null, now)).toBe('')
  })
})

describe('activityDot (one liveness slot)', () => {
  const attached = (turnActive) => ({ attached: true, sessionId: 's_1', status: 'ready', turnActive, threadStatus: turnActive ? 'working' : 'idle' })
  const detached = { attached: false, detached: true }
  const DETACHED_TITLE = "Detached: nano's session ended — click to open history"

  it('nano-attached idle → idle state, dot titled with the session', () => {
    const item = { ...byId.nth_codex_main, runtime: attached(false) }
    expect(activityDot(item)).toEqual({ state: 'idle', title: 'Active: nano session' })
    expect(rowRenderModel(item).dot).toEqual({ state: 'idle', title: 'Active: nano session' })
  })

  it('nano-attached mid-turn → working state, noted in the title', () => {
    const item = { ...byId.nth_codex_main, runtime: attached(true) }
    expect(activityDot(item)).toEqual({ state: 'working', title: 'Active: nano session (working)' })
  })

  it('keeps an idle parent idle when descendants are active', () => {
    const item = {
      ...byId.nth_codex_main,
      runtime: attached(false),
      subagentActivity: { total: 3, active: 2, workers: 2 },
    }
    expect(activityDot(item)).toEqual({ state: 'idle', title: 'Active: nano session' })
  })

  it('marks a working provider child on its own row', () => {
    const item = { ...byId.nth_codex_main, kind: 'subagent', fleet: { label: 'working' } }
    expect(activityDot(item)).toEqual({ state: 'working', title: 'Active: worker running' })
  })

  it('detached → its own state and tooltip (the row is one click from resuming)', () => {
    const item = { ...byId.nth_codex_main, runtime: detached }
    expect(activityDot(item)).toEqual({ state: 'detached', title: DETACHED_TITLE })
  })

  it('a stored thread still starting after a restart shows the starting dot, a failed one stays detached', () => {
    const starting = { ...byId.nth_codex_main, runtime: { ...detached, status: 'starting' } }
    expect(activityDot(starting)).toEqual({ state: 'starting', title: 'Active: nano session (starting)' })
    expect(activityDot({ ...byId.nth_codex_main, runtime: detached })).toEqual({ state: 'detached', title: DETACHED_TITLE })
  })

  it('live nowhere and remembered nowhere → null', () => {
    expect(activityDot(byId.nth_codex_main)).toBeNull()
    expect(activityDot({ ...byId.nth_codex_main, runtime: { attached: false, detached: false } })).toBeNull()
    expect(rowRenderModel(byId.nth_codex_main).dot).toBeNull()
  })

  it('liveness stays out of the health badges', () => {
    const item = { ...byId.nth_codex_archived, runtime: attached(false) }
    expect(rowRenderModel(item).badges).toEqual([{ tone: 'neutral', text: 'archived' }])
  })
})

describe('programmaticBadge', () => {
  const item = (interaction, origin) => ({ interaction, metadata: { origin: { value: origin } } })

  it('maps the verbatim origin value to its short label', () => {
    expect(programmaticBadge(item('programmatic', 'codex_exec'))).toBe('exec')
    expect(programmaticBadge(item('programmatic', 'sdk-cli'))).toBe('sdk')
    expect(programmaticBadge(item('interactive', 'cli'))).toBeNull()
    expect(programmaticBadge(item('unclassified', 'mtv5-probe'))).toBeNull()
  })

  it('renders an unlabeled programmatic origin verbatim rather than guessing', () => {
    expect(programmaticBadge(item('programmatic', 'future-batch-runner'))).toBe('future-batch-runner')
    expect(programmaticBadge({ interaction: 'programmatic', metadata: {} })).toBe('run')
  })
})

// --- Live working elapsed (one projection, two surfaces) ---------------------

describe('workingIndicator', () => {
  const NOW = 1_800_000_000_000

  it('is absent for anything that is not an attached, working session', () => {
    expect(workingIndicator(null)).toBeNull()
    expect(workingIndicator({})).toBeNull()
    expect(workingIndicator({ runtime: { detached: true } })).toBeNull()
    expect(workingIndicator({ runtime: { attached: true } })).toBeNull()
    expect(workingIndicator({ runtime: { status: 'ready', attached: true, turnActive: true, threadStatus: 'working', waiting: false }, x: 1 }).state).toBe('working')
  })

  it('formats the elapsed against the server anchor', () => {
    const item = { runtime: { status: 'ready', attached: true, turnActive: true, threadStatus: 'working', turnStartedAtMs: NOW - 125_000 } }
    expect(workingIndicator(item, NOW).text).toBe('2m 5s')
  })

  // D2/D8: a working row with no anchor states that it is working and says
  // nothing about how long — it does not fall back to "now".
  it('yields an empty text rather than a fabricated start', () => {
    const item = { runtime: { status: 'ready', attached: true, turnActive: true, threadStatus: 'working' } }
    expect(workingIndicator(item, NOW)).toEqual({
      state: 'working',
      text: '',
      title: 'Working — start time unknown',
    })
  })

  // D15, and the reason `waiting` had to become a payload field at all.
  it('reports waiting instead of a clock while a dialog is parked', () => {
    const item = { runtime: { status: 'ready', attached: true, turnActive: true, threadStatus: 'needs_you', turnStartedAtMs: NOW - 125_000, waiting: true } }
    expect(workingIndicator(item, NOW).state).toBe('waiting')
    expect(workingIndicator(item, NOW).text).toBe('waiting')
    expect(tickingAnchor(item)).toBe(0)
  })


  it('refuses an unusable anchor', () => {
    for (const turnStartedAtMs of [0, -1, null, undefined, NaN, 'soon']) {
      expect(tickingAnchor({ runtime: { status: 'ready', attached: true, turnActive: true, threadStatus: 'working', turnStartedAtMs } })).toBe(0)
    }
  })
})

// D6: tick to the next visible change, never one global interval. Past ten
// seconds only whole seconds move, and waking on the boundary is also what
// keeps a screenful of rows from drifting out of step with each other.
describe('nextTickDelayMs', () => {
  it('uses a tenth of a second while a tenth is displayed', () => {
    expect(nextTickDelayMs(0)).toBe(100)
    expect(nextTickDelayMs(9_999)).toBe(100)
  })

  it('lands on the next whole second afterwards', () => {
    expect(nextTickDelayMs(10_000)).toBe(1000)
    expect(nextTickDelayMs(10_300)).toBe(700)
    expect(nextTickDelayMs(65_999)).toBe(1)
  })
})

describe('activityDot waiting state', () => {
  it('separates waiting from working on the shared indicator', () => {
    const working = activityDot({ runtime: { status: 'ready', attached: true, turnActive: true, threadStatus: 'working' } })
    const waiting = activityDot({ runtime: { status: 'ready', attached: true, turnActive: true, threadStatus: 'needs_you', waiting: true } })
    expect(working.state).toBe('working')
    expect(waiting.state).toBe('waiting')
    expect(waiting.title).toContain('waiting for you')
  })

  it('leaves an idle attached session alone', () => {
    expect(activityDot({ runtime: { status: 'ready', attached: true, waiting: true } }).state).toBe('idle')
  })

})
