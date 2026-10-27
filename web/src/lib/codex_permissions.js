// The simplified permissions dial for codex starts (the operator, 2026-07-31): the New
// Window dialog speaks the profile vocabulary codex itself advertises
// (`permissionProfile/list`: :read-only / :workspace / :danger-full-access)
// instead of the sandbox × approval pair. The WIRE is unchanged — thread/start
// still takes the pair, the preview pane still shows it — this module is only
// the mapping between the one dial and the two parameters.
//
// Every profile maps to approval "never": the operator does not use permission
// prompts, and a posture that wants them is exactly what the Custom escape
// below preserves.
export const CODEX_PERMISSION_PROFILES = [
  { id: ':read-only', label: 'Read-only', sandbox: 'read-only', approval: 'never' },
  { id: ':workspace', label: 'Workspace', sandbox: 'workspace-write', approval: 'never' },
  { id: ':danger-full-access', label: 'Full access', sandbox: 'danger-full-access', approval: 'never' },
]

export const CUSTOM_PROFILE = 'custom'

// profileForPair answers which profile a stored (sandbox, approval) pair
// belongs to — or CUSTOM_PROFILE for anything outside the table (e.g. a saved
// default with approvals on). Custom keeps the raw pair rather than silently
// rewriting a posture the operator chose on purpose.
export function profileForPair(sandbox, approval) {
  const match = CODEX_PERMISSION_PROFILES.find((p) => p.sandbox === sandbox && p.approval === approval)
  return match?.id ?? CUSTOM_PROFILE
}

// pairForProfile answers the wire parameters for a chosen profile; null for
// Custom, which means "leave the pair as it is".
export function pairForProfile(id) {
  const match = CODEX_PERMISSION_PROFILES.find((p) => p.id === id)
  return match ? { sandbox: match.sandbox, approval: match.approval } : null
}
