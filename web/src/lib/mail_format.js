// Pure presentation rules for the durable-inbox surfaces, kept out
// of Svelte so the badge and status contracts are unit-testable.

// The sidebar badge is always unread/total (0/13 when nothing is unread), in
// one of two colors: yellow when the inbox needs attention — unread mail, or
// mail the agent read and never dispositioned (a dropped ball) — and dim gray
// when everything is handled. Operator simplified it to this two-state rule.
export function mailBadge(counts) {
  if (!counts || !counts.total) return null
  const unread = counts.unread ?? 0
  const seen = counts.seen ?? 0
  const parts = [`${unread} unread of ${counts.total} message${counts.total === 1 ? '' : 's'}`]
  if (seen > 0) parts.push(`${seen} read but never dispositioned`)
  return {
    text: `${unread}/${counts.total}`,
    state: unread > 0 || seen > 0 ? 'attention' : 'quiet',
    title: parts.join(' · '),
  }
}

export const MAIL_STATUSES = ['unread', 'seen', 'acted', 'declined', 'deferred']

// "seen" reads as an error state to the operator: the agent read the message
// and owes a disposition it has not given.
export function mailStatusLabel(status) {
  return status === 'seen' ? 'seen · undispositioned' : status
}

export function mailSenderLabel(sender) {
  if (!sender) return 'unknown'
  if (sender.kind === 'operator') return sender.name || 'Operator'
  if (sender.kind === 'system') return 'nano'
  return sender.name || sender.nativeThreadId || 'agent'
}

// Operator-authored mail is editable exactly while the recipient has not read
// it; after that the audit trail freezes the words the agent saw.
export function mailIsEditable(message) {
  return message?.sender?.kind === 'operator' && message?.status === 'unread'
}
