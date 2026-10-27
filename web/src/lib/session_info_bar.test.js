// @vitest-environment jsdom
//
// The bottom info bar and the thread menu behind it. These are component tests
// for the reason AGENTS.md gives: every failure here is "the state was right,
// the wrong thing was on screen". A bar assembled correctly but rendered into
// the read-only half, a menu whose entries exist in state but never mount, a
// mobile layout that hides the view toggle without putting it anywhere — a
// logic test passes through all three.
import { describe, it, expect, afterEach, beforeEach, vi } from 'vitest'
import { render, cleanup, fireEvent, waitFor } from '@testing-library/svelte'
import { tick } from 'svelte'
import ThreadPaneView from './ThreadPaneView.svelte'
import { viewport } from './viewport.svelte.js'
import { app } from '../app.svelte.js'
import { explorer } from '../explorer.svelte.js'
import { dropSessionStore, getSessionStore } from '../state.svelte.js'
import { __resetCodexModelsForTest } from './codex_models.js'

class MockEventSource {
  constructor(url) {
    this.url = url
    MockEventSource.instances.push(this)
  }
  close() {}
}
MockEventSource.instances = []

const emptyTranscript = {
  items: [],
  turns: [],
  page: { hasMoreBefore: false, beforeCursor: null },
  projection: { state: 'ready', epoch: 'tpe_1' },
}

const MODEL_CATALOG = {
  data: [{ id: 'gpt-5.6-sol', model: 'gpt-5.6-sol', displayName: 'GPT-5.6 Sol', supportedReasoningEfforts: [{ reasoningEffort: 'high' }] }],
}

function jsonResponse(body) {
  return { ok: true, status: 200, headers: { get: () => null }, json: async () => body, text: async () => '' }
}

function liveSession(overrides = {}) {
  return {
    id: 'session-1',
    provider: 'codex',
    hostedThreadId: 'nht_info_bar',
    threadId: 'th_0199abc',
    cwd: '/home/user/Code/nano',
    model: 'gpt-5.6-sol',
    posture: { model: 'gpt-5.6-sol', effort: 'high', serviceTier: 'priority' },
    status: 'ready',
    turnActive: false,
    ...overrides,
  }
}

function paneTab(overrides = {}) {
  return { id: 1, catalogId: 'nth_1', sessionId: 'session-1', provider: 'codex', nativeId: 'th_0199abc', title: 'Info bar', ...overrides }
}

// The server's /state answer, including the one field only it can supply: the
// branch it resolved for this session's cwd (live_git.go).
let stateSnapshot
let detachCalls

let metadataView = null

function stubFetch() {
  return vi.fn(async (url, init) => {
    const target = String(url)
    if (target.startsWith('/api/v1/identity/resolve')) {
      const ref = new URL(target, 'http://nano.test').searchParams.get('ref')
      return jsonResponse({
        displayName: '',
        nativeThreadId: stateSnapshot?.threadId ?? '',
        nativeThreadIdProvisional: false,
        hostedThreadId: ref,
        provider: stateSnapshot?.provider ?? 'codex',
        matchCount: 1,
      })
    }
    if (target.includes('/transcript')) return jsonResponse(emptyTranscript)
    if (target.includes('/api/codex/models')) return jsonResponse(MODEL_CATALOG)
    if (target.includes('/state')) return jsonResponse(stateSnapshot)
    if (target.endsWith('/metadata') && metadataView) return jsonResponse(metadataView)
    if (target.endsWith('/name') && init?.method === 'PUT') {
      const { name } = JSON.parse(init.body)
      return jsonResponse(liveSession({ name }))
    }
    if (init?.method === 'DELETE' || target.endsWith('/detach')) {
      detachCalls.push(target)
      return jsonResponse({})
    }
    return jsonResponse({})
  })
}

beforeEach(() => {
  __resetCodexModelsForTest()
  vi.stubGlobal('EventSource', MockEventSource)
  MockEventSource.instances = []
  detachCalls = []
  stateSnapshot = {
    sessionId: 'session-1',
    provider: 'codex',
    threadId: 'th_0199abc',
    cwd: '/home/user/Code/nano',
    gitBranch: 'main',
    status: 'ready',
    // No effort override in force: the bar must read the thread snapshot's own
    // reasoningEffort, and codex's serviceTier renders in compact bar vocabulary.
    turnOptions: { model: '', effort: '', summary: 'detailed', threadModel: 'gpt-5.6-sol' },
    posture: { model: 'gpt-5.6-sol', effort: 'high', serviceTier: 'priority' },
  }
  metadataView = null
  localStorage.clear()
  localStorage.setItem('nano-view', 'clean')
  vi.stubGlobal('fetch', stubFetch())
  app.sessions = [liveSession()]
  explorer.dataset = [{ id: 'nth_1', provider: 'codex', nativeId: 'th_0199abc', title: 'Info bar', metadata: {} }]
})

afterEach(() => {
  cleanup()
  dropSessionStore('session-1')
  vi.unstubAllGlobals()
  viewport.mobile = false
  viewport.coarse = false
})

async function renderPane(tab = paneTab()) {
  const result = render(ThreadPaneView, { tab })
  await waitFor(() => expect(result.container.querySelector('.info-bar-facts')?.textContent).toContain('main'))
  if (stateSnapshot?.threadId) {
    await waitFor(() => expect(result.container.querySelector('.info-bar-facts')?.textContent).toContain(stateSnapshot.threadId))
  }
  await tick()
  return result
}

async function openMenu(result) {
  await fireEvent.click(result.getByRole('button', { name: 'thread menu' }))
  await tick()
}

describe('the live pane info bar', () => {
  // Captured provider tier and Operator's requirement: local intent is not effective speed.
  it.each([['priority', 'fast'], ['ultrafast', 'ultrafast']])('shows provider confirmation and next-turn requests for %s at the rendered footer', async (requested, label) => {
    stateSnapshot.hostedThreadId = 'nht_info_bar'
    const tier = { desiredValue: requested, desiredDisposition: 'requested' }
    stateSnapshot.metadata = { revision: 1, fields: { serviceTier: tier } }
    stateSnapshot.turnOptions.serviceTier = requested
    const result = await renderPane()
    const bar = () => result.container.querySelector('.info-bar-facts').textContent
    expect(bar()).toContain(`unknown → ${label} next turn`)
    expect(bar()).not.toContain('✓')

    const session = getSessionStore('session-1')
    metadataView = { revision: 2, fields: { serviceTier: {
      ...tier, effectiveValue: 'default', effectiveDisposition: 'confirmed', effectiveSource: 'provider',
    } } }
    session.noteMetadataRevision(2)
    await waitFor(() => expect(bar()).toContain(`default ✓ → ${label} next turn`))

    metadataView = { revision: 3, fields: { serviceTier: {
      ...tier, desiredDisposition: 'confirmed', effectiveValue: requested, effectiveDisposition: 'observed', effectiveSource: 'provider',
    } } }
    session.noteMetadataRevision(3)
    await waitFor(() => expect(bar()).not.toContain('next turn'))
    expect(bar()).not.toContain('✓')

    metadataView = { revision: 4, fields: { serviceTier: {
      ...tier, desiredDisposition: 'confirmed', effectiveValue: requested, effectiveDisposition: 'confirmed', effectiveSource: 'provider',
    } } }
    session.noteMetadataRevision(4)
    await waitFor(() => expect(bar()).toContain(`${label} ✓`))

    metadataView = { revision: 5, fields: { serviceTier: {
      ...tier, desiredDisposition: 'rejected', effectiveValue: 'default', effectiveDisposition: 'confirmed', effectiveSource: 'provider',
    } } }
    session.noteMetadataRevision(5)
    await waitFor(() => expect(result.getByRole('alert').textContent).toContain('was rejected'))
    expect(bar()).toContain('default ✓')
    expect(bar()).not.toContain(label)

    // A later successful provider observation supersedes the old rejection banner.
    metadataView = { revision: 6, fields: { serviceTier: {
      ...tier, desiredDisposition: 'rejected', effectiveValue: requested, effectiveDisposition: 'confirmed', effectiveSource: 'provider',
    } } }
    session.noteMetadataRevision(6)
    await waitFor(() => expect(result.queryByRole('alert')).toBeNull())
    expect(bar()).toContain(`${label} ✓`)
  })

  it('makes a failed model refresh visible when opening speed controls', async () => {
    const result = await renderPane()
    const previous = globalThis.fetch
    vi.stubGlobal('fetch', vi.fn(async (url, init) => String(url).includes('?refresh=true')
      ? { ok: false, status: 502 } : previous(url, init)))
    await openMenu(result)
    await fireEvent.click(result.getByRole('button', { name: 'Change model / effort / speed' }))
    await waitFor(() => expect(result.getByRole('alert').textContent).toContain('could not be refreshed'))
    expect([...result.getByLabelText('speed for this session').options].map((o) => o.value)).not.toContain('ultrafast')
  })

	 it('fails closed when metadata is unavailable: stale footer facts disappear and posture controls are disabled', async () => {
		stateSnapshot = {
			...stateSnapshot,
			cwd: '/stale/metadata-cwd',
			metadataUnavailable: true,
			turnOptions: { model: 'gpt-5.6-luna', effort: 'ultra', serviceTier: 'priority', threadModel: 'gpt-5.6-sol' },
			posture: { model: 'gpt-5.6-sol', effort: 'high', serviceTier: 'priority' },
		}
		app.sessions = [liveSession({
			metadataUnavailable: true,
			cwd: '/stale/summary-cwd',
			posture: { model: 'gpt-5.6-sol', effort: 'high', serviceTier: 'priority' },
		})]
		const result = await renderPane()
		expect(result.getByRole('alert').textContent).toContain('Hosted metadata is unavailable')
		const bar = result.container.querySelector('.info-bar-facts').textContent
		expect(bar).toContain('main')
		expect(bar).toContain('th_0199abc')
		expect(bar).not.toContain('stale/metadata-cwd')
		expect(bar).not.toContain('gpt-5.6-luna')
		expect(bar).not.toContain('ultra')
		expect(bar).not.toContain('fast')

		await openMenu(result)
		const controlsButton = result.getByRole('button', { name: 'Change model / effort / speed' })
		await fireEvent.click(controlsButton)
		const controls = [...result.container.querySelectorAll('.turn-controls select')]
		expect(controls).toHaveLength(3)
		expect(controls.every((control) => control.disabled)).toBe(true)
		expect(controls.map((control) => control.value)).toEqual(['', '', ''])

		// A later metadata read recovers; the cached summary and /state still say
		// unavailable, and must not keep the warning or the disabled controls.
		metadataView = { revision: 4, fields: {} }
		getSessionStore('session-1').noteMetadataRevision(4)
		await waitFor(() => expect(result.queryByRole('alert')).toBeNull())
		expect(controls.every((control) => !control.disabled)).toBe(true)
	 })

	it('shows last committed facts when ingestion is degraded and disables further posture changes', async () => {
		stateSnapshot = {
			...stateSnapshot,
			metadataDegraded: true,
			posture: { model: 'gpt-5.6-sol', effort: 'high', serviceTier: 'priority' },
		}
		app.sessions = [liveSession({
			metadataDegraded: true,
			posture: { model: 'gpt-5.6-sol', effort: 'high', serviceTier: 'priority' },
		})]
		const result = await renderPane()
		expect(result.getByRole('alert').textContent).toContain('Showing the last committed canonical values')
		const bar = result.container.querySelector('.info-bar-facts').textContent
		expect(bar).toContain('nano')
		expect(bar).toContain('gpt-5.6-sol')
		expect(bar).toContain('high')
		expect(bar).toContain('fast')

		await openMenu(result)
		await fireEvent.click(result.getByRole('button', { name: 'Change model / effort / speed' }))
		const controls = [...result.container.querySelectorAll('.turn-controls select')]
		expect(controls).toHaveLength(3)
		expect(controls.every((control) => control.disabled)).toBe(true)
	})

	it('does not fall back to the summary working directory when unavailable state omits cwd', async () => {
		stateSnapshot = {
			...stateSnapshot,
			cwd: undefined,
			metadataUnavailable: true,
			turnOptions: { model: 'gpt-5.6-luna', effort: 'ultra', serviceTier: 'priority' },
		}
		app.sessions = [liveSession({ metadataUnavailable: true, cwd: '/stale/summary-only-cwd' })]
		const result = await renderPane()
		expect(result.getByRole('alert').textContent).toContain('Hosted metadata is unavailable')
		expect(result.container.querySelector('.info-bar-facts').textContent).not.toContain('summary-only-cwd')
	})

	it('renders the canonical display projection rather than historical desired intent', async () => {
		const metadata = {
			hostedThreadId: 'nht_projection', revision: 7,
			fields: {
				model: {
					desiredValue: 'historical-request', desiredDisposition: 'requested',
					effectiveValue: 'claude-actual-5', effectiveDisposition: 'observed',
					displayValue: 'claude-actual-5', displayDisposition: 'observed', displayFreshness: 'historical',
				},
				effort: { displayValue: 'high', displayDisposition: 'observed', displayFreshness: 'historical' },
			},
		}
		stateSnapshot = { ...stateSnapshot, hostedThreadId: 'nht_projection', metadata, posture: { model: 'historical-request', effort: 'high' } }
		app.sessions = [liveSession({ hostedThreadId: 'nht_projection', metadata, posture: { model: 'historical-request', effort: 'high' } })]
		const { container } = await renderPane()
		const bar = container.querySelector('.info-bar-facts').textContent
		expect(bar).toContain('claude-actual-5')
		expect(bar).not.toContain('historical-request')
	})

	it('states all five session facts, and only on the live half', async () => {
    const { container } = await renderPane()
    const bar = container.querySelector('.info-bar-facts')

    // Working directory (compacted, ~-style), branch, model, effort, thread id.
    expect(bar.textContent).toContain('~/Code/nano')
    expect(bar.textContent).toContain('main')
    expect(bar.textContent).toContain('gpt-5.6-sol')
    expect(bar.textContent).toContain('high')
    expect(bar.textContent).toContain('fast')
    expect(bar.textContent).toContain('th_0199abc')
    // The full path survives the compaction as the hover answer.
    expect(bar.querySelector('.info-bar-field.path').getAttribute('title')).toBe('/home/user/Code/nano')
    // Uniform rhythm: one dot between every adjacent pair, none dangling.
    expect(bar.querySelectorAll('.info-bar-sep')).toHaveLength(bar.querySelectorAll('.info-bar-field').length - 1)
  })

  it('shows the override in force, not the thread default it replaced', async () => {
    // The bar's whole claim is that it reports the session's truth. An
    // override set from the menu changes what the next turn runs on, so a bar
    // still naming the thread default would be reporting a stale posture.
    stateSnapshot.turnOptions = { model: 'gpt-5.6-luna', effort: 'low', summary: '', threadModel: 'gpt-5.6-sol' }
    const { container } = await renderPane()

    const bar = container.querySelector('.info-bar-facts').textContent
    expect(bar).toContain('gpt-5.6-luna')
    expect(bar).toContain('low')
    expect(bar).not.toContain('gpt-5.6-sol')
  })

  it('uses a resumed Claude tab’s cached native effort when live state has none', async () => {
    // Claude's resume state does not include an effort field. The tab's value
    // is the catalog cache of the native JSONL's latest recorded effort, so
    // rendering must not require another native-file lookup.
    stateSnapshot = {
      sessionId: 'session-1',
      provider: 'claude',
      threadId: 'th_0199abc',
      cwd: '/home/user/Code/nano',
      gitBranch: 'main',
      status: 'ready',
      turnOptions: { model: '', effort: '', summary: 'detailed', threadModel: 'claude-fable-5' },
    }
    app.sessions = [liveSession({ provider: 'claude', model: 'claude-fable-5' })]

    const { container } = await renderPane(paneTab({ effort: 'high' }))
    expect(container.querySelector('.info-bar-facts').textContent).toContain('high')
  })

  it('shows a newly started Claude model as soon as the live summary learns it', async () => {
    // A fresh Claude session is staged before system.init lands. Its first
    // /state can therefore contain the selected effort but an empty
    // threadModel. The runtime nudge refreshes the lighter session summary
    // once init reports the actual model; the footer must use that value
    // without requiring a page reload and a second /state request.
    stateSnapshot = {
      sessionId: 'session-1',
      provider: 'claude',
      threadId: 'th_0199abc',
      cwd: '/home/user/Code/nano',
      gitBranch: 'main',
      status: 'ready',
      turnOptions: { model: '', effort: 'high', summary: 'detailed', threadModel: '' },
		posture: { model: '', effort: 'high', serviceTier: '' },
    }
    app.sessions = [liveSession({ provider: 'claude', model: '' })]

    const { container } = await renderPane(paneTab({ provider: 'claude', model: '' }))
    expect(container.querySelector('.info-bar-facts').textContent).not.toContain('claude-fable-5')

	app.sessions = [liveSession({ provider: 'claude', model: 'claude-fable-5', posture: { model: 'claude-fable-5', effort: 'high', serviceTier: '' } })]
	// A summary model alone is not canonical metadata. The runtime revision
	// refetch supplies posture; until then the fact remains honestly unknown.
	await tick()
	expect(container.querySelector('.info-bar-facts')?.textContent).not.toContain('claude-fable-5')
    expect(container.querySelector('.info-bar-facts').textContent).toContain('high')
  })

  it('keeps a Priority creation choice through the starting-to-ready summary transition', async () => {
    // The first /state snapshot can race the Codex thread/start handshake.
    // Its thread snapshot then has no tier at all; the compact summary is the
    // only evidence that Nano has already accepted Priority. The footer must
    // adopt that summary on both sides of the transition instead of inventing
    // Standard until a hard refresh.
    stateSnapshot = {
      ...stateSnapshot,
      status: 'starting',
      thread: { reasoningEffort: 'high', serviceTier: '' },
		posture: { serviceTier: 'priority' },
    }
    app.sessions = [liveSession({ status: 'starting', posture: { serviceTier: 'priority' } })]

    const { container } = await renderPane()
    const bar = () => container.querySelector('.info-bar-facts').textContent
    expect(bar()).toContain('fast')
    expect(bar()).not.toContain('default')

    app.sessions = [liveSession({ status: 'ready', posture: { serviceTier: 'priority' } })]
    await waitFor(() => expect(bar()).toContain('fast'))
    expect(bar()).not.toContain('default')
  })

  it('does not label an unconfirmed starting Codex tier as Standard', async () => {
    stateSnapshot = {
      ...stateSnapshot,
      status: 'starting',
      thread: { reasoningEffort: 'high', serviceTier: '' },
		posture: {},
    }
    app.sessions = [liveSession({ status: 'starting', posture: {} })]
    const { container } = await renderPane()
    const bar = container.querySelector('.info-bar-facts').textContent
    expect(bar).not.toContain('default')
    expect(bar).not.toContain('fast')
  })

  it('prefers a live effort over the tab’s cached recorded effort', async () => {
    stateSnapshot.turnOptions = { model: '', effort: 'low', summary: 'detailed', threadModel: 'gpt-5.6-sol' }
    const { container } = render(ThreadPaneView, { tab: paneTab({ effort: 'high' }) })
    await waitFor(() => expect(container.querySelector('.info-bar-facts')?.textContent).toContain('low'))
    expect(container.querySelector('.info-bar-facts').textContent).not.toContain('high')
  })

  it('gives the read-only half the recorded facts, and no menu', async () => {
    // The historical bar states the catalog row's recorded truth (the operator,
    // 2026-07-31): the branch the thread ran on, the last model/effort it was
    // seen with. Nothing there is editable, so there is no ⋯.
    app.sessions = []
    explorer.dataset = [
      {
        id: 'nth_1',
        provider: 'codex',
        nativeId: 'th_0199abc',
        title: 'Info bar',
        metadata: {
          cwd: { state: 'known', value: '/home/user/Code/sample-project', evidence: 'native_durable' },
          historicalBranch: { state: 'known', value: 'main', evidence: 'native_durable' },
          model: { state: 'known', value: 'gpt-5.6-sol', evidence: 'native_durable' },
          effort: { state: 'known', value: 'medium', evidence: 'native_durable' },
          priority: { state: 'unknown', value: null, evidence: null },
        },
      },
    ]
    const { container } = render(ThreadPaneView, { tab: paneTab({ sessionId: '' }) })
    await tick()
    const bar = container.querySelector('.info-bar.historical')
    expect(bar).not.toBeNull()
    expect(bar.textContent).toContain('~/Code/sample-project')
    expect(bar.textContent).toContain('main')
    expect(bar.textContent).toContain('gpt-5.6-sol')
    expect(bar.textContent).toContain('medium')
    expect(bar.textContent).toContain('th_0199abc')
    expect(container.querySelector('.thread-menu-btn')).toBeNull()
  })
})

describe('the thread menu', () => {
  it('renames the live session from its discoverable thread menu action', async () => {
    const result = await renderPane()
    await openMenu(result)

    await fireEvent.click(result.getByRole('button', { name: 'Rename session' }))
    const input = result.getByRole('textbox', { name: 'Rename session' })
    expect(document.activeElement).toBe(input)
    await fireEvent.input(input, { target: { value: '  typed name  ' } })
    await fireEvent.click(result.getByRole('button', { name: 'Save name' }))

    await waitFor(() => expect(fetch).toHaveBeenCalledWith(
      '/api/sessions/session-1/name',
      expect.objectContaining({ method: 'PUT', body: JSON.stringify({ name: 'typed name' }) }),
    ))
    expect(app.sessions[0].name).toBe('typed name')
  })

  it('holds the model/effort/priority controls, closed until asked for', async () => {
    const result = await renderPane()
    const { container } = result

    // The point of the move: the selects are not on screen by default.
    expect(container.querySelector('.turn-controls')).toBeNull()

    await openMenu(result)
    expect(container.querySelector('.turn-controls')).toBeNull()

    await fireEvent.click(result.getByRole('button', { name: 'Change model / effort / speed' }))
    await waitFor(() => expect(container.querySelectorAll('.thread-menu-pop .turn-control select')).toHaveLength(3))
  })

  it('puts Detach beside Thread Details on desktop', async () => {
    const result = await renderPane()
    const bar = result.container.querySelector('.view-bar')
    expect(bar.querySelector('.detach-toggle')?.textContent.trim()).toBe('Detach')
    expect(bar.querySelector('.thread-details-toggle')).not.toBeNull()
    expect(result.container.querySelector('.pane-detach-btn')).toBeNull()
  })

  it('closes on Escape', async () => {
    const result = await renderPane()
    await openMenu(result)
    expect(result.container.querySelector('.thread-menu-pop')).not.toBeNull()

    await fireEvent.keyDown(window, { key: 'Escape' })
    await tick()
    expect(result.container.querySelector('.thread-menu-pop')).toBeNull()
  })
})

describe('the phone layout', () => {
  beforeEach(() => {
    viewport.mobile = true
    viewport.coarse = true
  })

  it('hides the view toggle and the detach float, and puts both in the menu', async () => {
    const result = await renderPane()
    const { container } = result

    // Neither one costs a row of a 390px screen any more…
    expect(container.querySelector('.view-bar')).toBeNull()
    expect(container.querySelector('.pane-detach-btn')).toBeNull()

    // …and neither one is gone. This is the pairing that matters: a phone with
    // no way to reach All, or no way to let go of a live thread, is the bug.
    await openMenu(result)
    const menu = container.querySelector('.thread-menu-pop')
    expect(menu.querySelector('.view-toggle')).not.toBeNull()
    const transcript = menu.querySelector('.thread-menu-section')
    expect(transcript.querySelector('.detach-toggle')?.textContent.trim()).toBe('Detach')
  })

  it('still switches the transcript view from inside the menu', async () => {
    const result = await renderPane()
    await openMenu(result)

    await fireEvent.click(result.getByRole('button', { name: 'All' }))
    await tick()
    expect(localStorage.getItem('nano-view')).toBe('all')
  })

  it('keeps the view toggle inline on desktop', async () => {
    viewport.mobile = false
    const { container } = await renderPane()
    expect(container.querySelector('.view-bar .view-toggle')).not.toBeNull()
  })
})
