import { TRANSCRIPT_CATEGORIES } from './transcript_query.js'
import { fmtTurnDuration } from './lib/shared/duration_format.js'

const CATEGORY_LABELS = new Map(TRANSCRIPT_CATEGORIES.map((category) => [category.key, category.label]))

export const TRANSCRIPT_AUTO_PAGE_THRESHOLD = 120

// Markdown rendering moved to lib/transcript/markdown.js + lib/transcript/message_body.js — the one
// display engine shared with the live conversation view (the operator 2026-07-23). Its
// unsafe-link filtering carried over there.

export function itemText(item) {
  if (item?.text) return item.text
  if (item?.preview) return item.preview
  return (item?.blocks ?? [])
    .map((block) => block.text ?? block.preview ?? '')
    .filter(Boolean)
    .join('\n')
}

// How long each completed turn took, keyed by the item that ENDED it, so a
// transcript can hang the duration off the message that closes the turn.
//
// Native evidence only. The durations come from the provider's own
// `task_complete.duration_ms` (codex) and `turn_duration` system row (claude);
// a turn without one gets no entry rather than a subtraction of two row
// timestamps, which would silently bill queueing and idle time as work. A
// turn under a second gets no entry either — fmtTurnDuration returns '' and a
// "0s" parenthetical is noise, not information. Missing durations are the
// common case on both providers, so this map is usually sparse (see
// history.ThreadTurnTotals for the corpus measurements).
export function turnDurationsByFinalItem(turns) {
  const byItem = new Map()
  for (const turn of turns ?? []) {
    if (!turn?.finalItemId) continue
    const label = fmtTurnDuration(turn.durationMs)
    if (label) byItem.set(turn.finalItemId, label)
  }
  return byItem
}

export function transcriptItemCategory(item) {
  if (item?.category && CATEGORY_LABELS.has(item.category)) return item.category
  if (item?.sidechain) return 'subagent'
	if (item?.type === 'user_message' || item?.type === 'agent_message' || item?.type === 'interaction') return 'messages'
  if (item?.type === 'reasoning') return 'reasoning'
  if (item?.type === 'tool_result') return 'toolResult'
  if (['command', 'tool_call', 'file_change', 'plan'].includes(item?.type)) return 'toolCall'
  if (item?.type === 'notice' && (item?.status === 'error' || item?.filterTags?.includes('error'))) return 'error'
  if (['notice', 'compaction'].includes(item?.type)) return 'system'
  return 'unclassified'
}

// Live projection items use a provider-neutral camelCase vocabulary while
// transcriptItemCategory above knows the durable projection's snake_case
// taxonomy. This presentation map lands both DTO shapes on the same category
// keys, which lets one hide-set filter the durable and live halves identically.
// Tool results remain attached to their tool item in the live DTO.
const LIVE_TYPE_CATEGORIES = {
  userMessage: 'messages',
  agentMessage: 'messages',
  reasoning: 'reasoning',
  commandExecution: 'toolCall',
  fileChange: 'toolCall',
  mcpToolCall: 'toolCall',
  dynamicToolCall: 'toolCall',
  webSearch: 'toolCall',
  plan: 'toolCall',
  toolUse: 'toolCall',
  subAgentActivity: 'subagent',
  contextCompaction: 'system',
}

// Unknown types remain available in All, but Enhanced hides their category.
export function liveItemCategory(item) {
  return LIVE_TYPE_CATEGORIES[item?.type] ?? 'unclassified'
}

export function transcriptCategoryLabel(itemOrCategory) {
  const category = typeof itemOrCategory === 'string'
    ? itemOrCategory
    : transcriptItemCategory(itemOrCategory)
  return CATEGORY_LABELS.get(category) ?? 'Unclassified Provider Events'
}

const DURABLE_TYPE_LABELS = {
  reasoning: 'reasoning',
  command: 'shell',
  tool_call: 'tool call',
  tool_result: 'tool result',
  file_change: 'file change',
  plan: 'plan',
  compaction: 'compaction',
  notice: 'notice',
  provider_event: 'provider event',
  interaction: 'interaction',
}

// Kind label for a durable (projected) transcript item — shared by the
// Explorer's historical transcript and the live view's resumed-history block.
export function transcriptItemLabel(item) {
  if (item.type === 'interaction') return item.role === 'user' ? 'you · response' : 'agent · question'
  if (item.type === 'user_message' || item.role === 'user') return 'you'
  if (item.type === 'agent_message') {
    return `${item.sidechain ? 'sub-agent' : 'agent'}${item.isFinalAnswer || item.phase === 'final_answer' ? ' · final' : ''}`
  }
  return DURABLE_TYPE_LABELS[item.type] ?? transcriptCategoryLabel(item)
}

export function transcriptItemKind(item) {
	if (item?.type === 'interaction') return item?.role === 'user' ? 'user' : 'agent'
  if (item?.type === 'user_message' || item?.role === 'user') return 'user'
  if (item?.type === 'agent_message') return 'agent'
  if (transcriptItemCategory(item) === 'error') return 'error'
  return 'aux'
}

export function formatTranscriptTimestamp(value) {
  if (!value) return ''
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return ''
  return new Intl.DateTimeFormat(undefined, {
    month: 'short',
    day: 'numeric',
    hour: 'numeric',
    minute: '2-digit',
    second: '2-digit',
  }).format(date)
}

// Agent submission time uses the viewer's local zone in Enhanced view.
// Live rows use the server-recorded UTC operation value; saved rows recover
// it from the sender footer. Formatting never rewrites the prompt text.
export function formatSubmissionTimestamp(value) {
  if (!value) return ''
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return ''
  return new Intl.DateTimeFormat('en-US', {
    month: 'short',
    day: 'numeric',
    hour: 'numeric',
    minute: '2-digit',
    second: '2-digit',
    timeZoneName: 'short',
  }).format(date)
}

// senderFooter emits UTC seconds. Only mail currently appends a notice after
// it, through Notices.Append's double-newline separator. This is presentation
// evidence, so a pasted footer has the same display as a delivered one.
export function senderSubmissionTimestampFromFooter(text) {
  if (typeof text !== 'string') return ''
  const footer = /\n\n\[from ([^\r\n]+) · ([^\s·\[\]]+) · submitted (\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z)\](?:\n\n\[nano: [1-9]\d* unread — GET \/api\/v1\/hosted-threads\/nht_[0-9a-f]{32}\/inbox\])?(?![\s\S])/.exec(text)
  return footer?.[3] ?? ''
}

export function shouldAutoLoadEarlier({
  scrollTop,
  hasOlder,
  loadingEarlier,
  earlierError,
}) {
  const top = Number(scrollTop)
  return Number.isFinite(top) &&
    top < TRANSCRIPT_AUTO_PAGE_THRESHOLD &&
    Boolean(hasOlder) &&
    !loadingEarlier &&
    !earlierError
}

export function normalizedItemMetadata(item) {
  return [
    ['type', item?.type],
    ['role', item?.role],
    ['category', transcriptItemCategory(item)],
    ['status', item?.status],
    ['phase', item?.phase],
    ['tool', item?.toolName],
    ['call', item?.callId],
    ['turn', item?.turnId],
    ['seq', item?.projectedSeq],
    ['id', item?.id],
  ].filter(([, value]) => value !== undefined && value !== null && value !== '')
}

export function evidenceDisplay(evidence) {
  if (!evidence) return { encoding: 'Unavailable', text: '(raw row unavailable)' }
  const raw = evidence.rawRow ?? evidence.raw ?? evidence.payload ?? evidence.sourceRow ?? null
  if (typeof raw === 'string') return { encoding: 'UTF-8 native row', text: raw }
  if (typeof evidence.rawBase64 === 'string' && evidence.rawBase64) {
    return {
      encoding: 'Base64 · exact native bytes (not decoded)',
      text: evidence.rawBase64,
    }
  }
  if (raw != null) return { encoding: 'Legacy JSON value', text: JSON.stringify(raw, null, 2) }
  return { encoding: 'Unavailable', text: '(raw row unavailable)' }
}

export function evidenceProvenance(evidence) {
  if (!evidence) return []
  return [
    ['provider', evidence.provider],
    ['path', evidence.sourcePath ?? evidence.path],
    ['line', evidence.sourceLine ?? evidence.line],
    ['row type', evidence.sourceRowType ?? evidence.rowType],
    ['payload', evidence.sourcePayloadType ?? evidence.payloadType],
    ['bytes', evidence.rawBytes],
    ['valid JSON', typeof evidence.validJson === 'boolean' ? String(evidence.validJson) : null],
    ['valid UTF-8', typeof evidence.validUtf8 === 'boolean' ? String(evidence.validUtf8) : null],
    ['SHA-256', evidence.sha256],
    ['projection', evidence.projectionKey],
  ].filter(([, value]) => value !== undefined && value !== null && value !== '')
}

export function consumeTranscriptFilterEscape(event, open, close) {
  if (!open || event?.key !== 'Escape') return false
  event.preventDefault()
  event.stopPropagation()
  event.stopImmediatePropagation?.()
  close()
  return true
}
