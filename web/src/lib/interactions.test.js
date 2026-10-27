// How a raw server request becomes a card. The mapping is the part most likely
// to drift from the schema, and the part a component test cannot inspect.
import { describe, it, expect } from 'vitest'
import {
  interactionActions,
  interactionCommand,
  interactionDetails,
  interactionExpired,
  interactionQuestions,
  interactionTitle,
  visibleInteractions,
} from './interactions.js'

const approval = (overrides = {}) => ({
  id: '1',
  method: 'item/commandExecution/requestApproval',
  kind: 'commandApproval',
  createdAt: '2026-07-30T10:00:00Z',
  params: { command: 'rm -rf build', cwd: '/workspace/nano', threadId: 't1' },
  ...overrides,
})

const question = (overrides = {}) => ({
  id: '2',
  method: 'item/tool/requestUserInput',
  kind: 'userInput',
  createdAt: '2026-07-30T10:00:01Z',
  params: {
    questions: [
      {
        id: 'q1',
        header: 'Style',
        question: 'Tabs or spaces?',
        options: [
          { label: 'Spaces (Recommended)', description: 'PEP 8 default' },
          { label: 'Tabs', description: 'Go default' },
        ],
      },
    ],
  },
  ...overrides,
})

describe('interaction card mapping', () => {
  it('renders an approval from the wire payload, not a nano paraphrase', () => {
    const a = approval()
    expect(interactionTitle(a)).toMatch(/command/i)
    expect(interactionCommand(a)).toBe('rm -rf build')
    expect(interactionDetails(a)).toContain('in /workspace/nano')
  })

  it('joins an argv-array command rather than rendering [object Object]', () => {
    expect(interactionCommand(approval({ params: { command: ['git', 'push', '--force'] } })))
      .toBe('git push --force')
  })

  it('offers Always allow only where a session-scoped grant exists', () => {
    const decisions = (i) => interactionActions(i).map((a) => a.decision)
    expect(decisions(approval())).toContain('acceptForSession')
    expect(decisions({ kind: 'permissions' })).toContain('acceptForSession')
    // A question is answered, not approved — "always allow a question" is
    // meaningless and its response shape has no room for it.
    expect(decisions(question())).toEqual(['accept', 'decline'])
    expect(decisions({ kind: 'elicitation' })).toEqual(['accept', 'decline', 'cancel'])
  })

  it('lifts the "(Recommended)" suffix out of the label into a badge (standard parity)', () => {
    const [q] = interactionQuestions(question())
    expect(q.options[0].recommended).toBe(true)
    expect(q.options[1].recommended).toBe(false)
    expect(q.freeText).toBe(false)
    expect(q.id).toBe('q1')
  })

  it('treats a question with no options — or isOther — as free text', () => {
    const [bare] = interactionQuestions(question({ params: { questions: [{ id: 'q1', question: 'Which branch?' }] } }))
    expect(bare.freeText).toBe(true)
    const [other] = interactionQuestions(
      question({ params: { questions: [{ id: 'q1', question: 'Which?', isOther: true, options: [{ label: 'a' }] }] } }),
    )
    expect(other.freeText).toBe(true)
  })

  it('narrows the buttons to what the request actually advertises', () => {
    // Observed live 2026-07-30: a real command approval offered
    // accept / acceptWithExecpolicyAmendment / cancel — no acceptForSession.
    const advertised = approval({
      params: {
        command: 'rm -rf build',
        availableDecisions: ['accept', { acceptWithExecpolicyAmendment: { execpolicy_amendment: ['x'] } }, 'cancel'],
      },
    })
    const decisions = interactionActions(advertised).map((a) => a.decision)
    expect(decisions).not.toContain('acceptForSession')
    // Accept and decline survive regardless: every shape has a yes and a no,
    // and a card with no way to refuse is worse than an ignored verb.
    expect(decisions).toContain('accept')
    expect(decisions).toContain('decline')
    // No advertisement at all leaves the kind's full set alone.
    expect(interactionActions(approval()).map((a) => a.decision)).toContain('acceptForSession')
  })

  it('collapses a request past its autoResolutionMs deadline', () => {
    const now = Date.parse('2026-07-30T10:00:30Z')
    const live = question({ expiresAt: '2026-07-30T10:01:00Z' })
    const dead = question({ id: '3', expiresAt: '2026-07-30T10:00:10Z' })
    expect(interactionExpired(live, now)).toBe(false)
    expect(interactionExpired(dead, now)).toBe(true)
    // No deadline at all is not "expired" — approvals park indefinitely.
    expect(interactionExpired(approval(), now)).toBe(false)
    expect(visibleInteractions([dead, live, approval()], now).map((i) => i.id)).toEqual(['1', '2'])
  })

})

// Claude's dialogs ride the SAME card. The mapping below is what makes that
// true: its wire shape differs from codex's in every detail that matters —
// questions nested under the tool input, keyed by text rather than by id — and
// this is the only place that difference is allowed to exist.
describe('claude requests through the shared card', () => {
  const claudeQuestion = (overrides = {}) => ({
    id: 'req-1',
    method: 'can_use_tool',
    kind: 'userInput',
    createdAt: '2026-07-30T10:00:00Z',
    params: {
      subtype: 'can_use_tool',
      tool_name: 'AskUserQuestion',
      input: {
        questions: [
          {
            question: 'Tabs or spaces?',
            header: 'Indentation',
            multiSelect: false,
            options: [{ label: 'Tabs', description: 'Indent with tabs (Recommended)' }, { label: 'Spaces' }],
          },
        ],
      },
    },
    ...overrides,
  })

  it('finds questions nested under the tool input, and uses their text as the id', () => {
    const [q] = interactionQuestions(claudeQuestion())
    // The CLI keys its answers by the question text; there are no question ids
    // on this side, so the text IS the id and the card's payload comes out
    // correct without the card knowing which provider it is rendering.
    expect(q.id).toBe('Tabs or spaces?')
    expect(q.header).toBe('Indentation')
    expect(q.freeText).toBe(false)
    expect(q.multiSelect).toBe(false)
    expect(q.options[0].recommended).toBe(true)
    expect(q.options[0].description).toBe('Indent with tabs')
  })

  it('carries multiSelect through so the card can offer more than one answer', () => {
    const request = claudeQuestion()
    request.params.input.questions[0].multiSelect = true
    expect(interactionQuestions(request)[0].multiSelect).toBe(true)
  })

  it('renders a tool permission with approve/deny only — there is no session grant to offer', () => {
    const permission = {
      id: 'req-2',
      method: 'can_use_tool',
      kind: 'toolPermission',
      createdAt: '2026-07-30T10:00:00Z',
      params: { subtype: 'can_use_tool', tool_name: 'Bash', display_name: 'Bash', description: 'List the tree', input: { command: 'ls -R' } },
    }
    expect(interactionTitle(permission)).toContain('Bash')
    expect(interactionDetails(permission)).toContain('List the tree')
    // The command block shows what would actually run, verbatim.
    expect(interactionCommand(permission)).toBe('ls -R')
    expect(interactionActions(permission).map((a) => a.decision)).toEqual(['accept', 'decline'])
  })

  it('shows a non-shell tool as the arguments it would run with rather than paraphrasing them', () => {
    const permission = {
      id: 'req-3',
      kind: 'toolPermission',
      params: { tool_name: 'Write', input: { file_path: '/tmp/x', contents: 'hi' } },
    }
    expect(interactionCommand(permission)).toContain('"file_path"')
  })

  it("shows grok's plan approval as the plan markdown, not JSON-escaped text", () => {
    const approval = {
      id: 'req-4',
      kind: 'toolPermission',
      params: {
        tool_name: 'exit_plan_mode',
        display_name: 'Exit plan mode',
        input: { plan: '# Plan\n\n- do the thing\n' },
      },
    }
    expect(interactionCommand(approval)).toBe('# Plan\n\n- do the thing\n')
  })
})
