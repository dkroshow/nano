// What the one dialog card renders, derived from a server-owned pending
// interaction. Kept out of the component so the mapping from a raw wire payload
// to buttons and body text is testable without a DOM — and so slice 3 can point
// claude's dialogs at the same card by adding kinds here.

export const DECISION_ACCEPT = 'accept'
export const DECISION_ACCEPT_FOR_SESSION = 'acceptForSession'
export const DECISION_DECLINE = 'decline'
export const DECISION_CANCEL = 'cancel'

const KIND_LABELS = {
  commandApproval: 'command',
  fileChangeApproval: 'file change',
  permissions: 'permissions',
  userInput: 'question',
  elicitation: 'mcp input',
  // Claude's `can_use_tool` for anything that is not a question: one tool, one
  // input, yes or no.
  toolPermission: 'tool',
}

export function interactionKindLabel(interaction) {
  return KIND_LABELS[interaction?.kind] ?? interaction?.kind ?? 'request'
}

export function interactionTitle(interaction) {
  switch (interaction?.kind) {
    case 'commandApproval':
      return 'Run this command?'
    case 'fileChangeApproval':
      return 'Apply these file changes?'
    case 'permissions':
      return 'Grant additional permissions?'
    case 'userInput':
      return 'The agent has a question'
    case 'elicitation':
      return `${interaction?.params?.serverName ?? 'An MCP server'} needs input`
    case 'toolPermission':
      return `Let the agent use ${interaction?.params?.display_name ?? interaction?.params?.tool_name ?? 'this tool'}?`
    default:
      return 'The agent needs a decision'
  }
}

// Detail lines under the title: the wire's own fields, never a nano paraphrase.
export function interactionDetails(interaction) {
  const p = interaction?.params ?? {}
  const out = []
  if (p.reason) out.push(p.reason)
  if (p.message) out.push(p.message)
  if (p.description) out.push(p.description)
  if (p.cwd) out.push(`in ${p.cwd}`)
  if (p.grantRoot) out.push(`grant write access under ${p.grantRoot}`)
  return out
}

// The monospace block, when the request has one thing to show verbatim.
export function interactionCommand(interaction) {
  const p = interaction?.params ?? {}
  if (typeof p.command === 'string') return p.command
  if (Array.isArray(p.command)) return p.command.join(' ')
  if (interaction?.kind === 'permissions' && p.permissions) return JSON.stringify(p.permissions, null, 2)
  // Claude carries the tool's arguments under `input`. A bash command reads as
  // itself; anything else is shown as the JSON the tool would actually run with,
  // because paraphrasing an arbitrary tool's arguments would hide what is being
  // approved.
  if (interaction?.kind === 'toolPermission' && p.input) {
    if (typeof p.input.command === 'string') return p.input.command
    // Grok's exit_plan_mode approval: the plan markdown IS the thing being
    // approved, so show it verbatim rather than as a JSON-escaped string.
    if (typeof p.input.plan === 'string') return p.input.plan
    return JSON.stringify(p.input, null, 2)
  }
  return ''
}

// Questions the card renders as option lists. `isOther` marks a free-text
// question; `options` may be absent, which is also free text.
//
// Two dialects, one shape. Codex puts questions at the top level and gives each
// one an `id`; claude's AskUserQuestion nests them under the tool `input` and
// has no ids at all — the CLI keys its answers by the question TEXT. So the
// question's text IS its id on that side, which keeps the card's answer payload
// (`{id: [labels]}`) correct for both without the card knowing which provider it
// is rendering.
export function interactionQuestions(interaction) {
  if (interaction?.kind !== 'userInput') return []
  const questions = interaction?.params?.questions ?? interaction?.params?.input?.questions ?? []
  return questions.map((q) => ({
    id: q.id ?? q.question,
    header: q.header ?? '',
    question: q.question ?? '',
    isSecret: Boolean(q.isSecret),
    multiSelect: Boolean(q.multiSelect),
    freeText: Boolean(q.isOther) || !(q.options ?? []).length,
    options: (q.options ?? []).map((o) => ({
      label: o.label ?? '',
      recommended: /\(recommended\)\s*$/i.test(o.label ?? '') || /\(recommended\)\s*$/i.test(o.description ?? ''),
      description: stripRecommended(o.description ?? ''),
    })),
  }))
}

function stripRecommended(text) {
  return String(text).replace(/\s*\(recommended\)\s*$/i, '').trim()
}

export function optionLabel(option) {
  return stripRecommended(option?.label ?? '')
}

function advertisedDecisions(interaction) {
  const raw = interaction?.params?.availableDecisions
  if (!Array.isArray(raw)) return null
  const names = raw
    .map((d) => (typeof d === 'string' ? d : Object.keys(d ?? {})[0]))
    .filter(Boolean)
  return names.length ? names : null
}

// interactionActions is the buttons the card shows: the kind's full set,
// narrowed to what this particular request advertises. Accept and decline
// always survive — every response shape has a yes and a no, and a card with no
// way to refuse would be worse than one offering a verb the server ignores.
export function interactionActions(interaction) {
  const actions = interactionActionsForKind(interaction)
  const advertised = advertisedDecisions(interaction)
  if (!advertised) return actions
  return actions.filter(
    (a) => a.decision === DECISION_ACCEPT || a.decision === DECISION_DECLINE || advertised.includes(a.decision),
  )
}

// "Always allow" only where a session-scoped grant exists; a question is
// answered, not approved, so it offers Send and Skip instead.
function interactionActionsForKind(interaction) {
  switch (interaction?.kind) {
    case 'commandApproval':
    case 'fileChangeApproval':
      return [
        { decision: DECISION_ACCEPT, label: 'Approve', tone: 'accept' },
        { decision: DECISION_ACCEPT_FOR_SESSION, label: 'Always allow', tone: 'plain' },
        { decision: DECISION_DECLINE, label: 'Deny', tone: 'deny' },
      ]
    case 'permissions':
      return [
        { decision: DECISION_ACCEPT, label: 'Grant', tone: 'accept' },
        { decision: DECISION_ACCEPT_FOR_SESSION, label: 'Grant for session', tone: 'plain' },
        { decision: DECISION_DECLINE, label: 'Deny', tone: 'deny' },
      ]
    case 'userInput':
      return [
        { decision: DECISION_ACCEPT, label: 'Send', tone: 'accept' },
        { decision: DECISION_DECLINE, label: 'Skip', tone: 'plain' },
      ]
    case 'elicitation':
      return [
        { decision: DECISION_ACCEPT, label: 'Accept', tone: 'accept' },
        { decision: DECISION_DECLINE, label: 'Decline', tone: 'plain' },
        { decision: DECISION_CANCEL, label: 'Cancel', tone: 'deny' },
      ]
    case 'toolPermission':
      // No "Always allow": claude's control channel has no session-scoped grant
      // to make, and offering one that silently degrades to a one-shot approve
      // would be a lie about what the click did.
      return [
        { decision: DECISION_ACCEPT, label: 'Allow', tone: 'accept' },
        { decision: DECISION_DECLINE, label: 'Deny', tone: 'deny' },
      ]
    default:
      return [
        { decision: DECISION_ACCEPT, label: 'Allow', tone: 'accept' },
        { decision: DECISION_DECLINE, label: 'Deny', tone: 'deny' },
      ]
  }
}

// A request the app-server will resolve on its own timer (autoResolutionMs, the
// one request kind that carries one). Past the deadline the card collapses
// silently: the answer no longer has anywhere to go, and a button that cannot
// work is worse than no button.
export function interactionExpired(interaction, now = Date.now()) {
  if (!interaction?.expiresAt) return false
  const deadline = Date.parse(interaction.expiresAt)
  return Number.isFinite(deadline) && deadline <= now
}

// visibleInteractions is what the pane renders: everything still answerable,
// oldest first. Resolving is server-owned, so a card disappears because the
// server said so — answered here, answered in another tab, or timed out.
export function visibleInteractions(interactions, now = Date.now()) {
  return (interactions ?? [])
    .filter((i) => i?.id && !interactionExpired(i, now))
    .sort((a, b) => String(a.createdAt).localeCompare(String(b.createdAt)))
}
