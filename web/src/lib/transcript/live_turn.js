// A small, local projection of Codex's typed wire activity. This is purposely
// not a second source of truth for whether a turn is active: turn/started and
// turn/completed still own that. It only gives an active turn a useful, human
// readable name while it streams.

function compactDetail(value) {
  if (typeof value !== 'string') return null
  const line = value.trim().split('\n')[0]
  if (!line) return null
  return line.length > 72 ? line.slice(0, 71) + '…' : line
}

export function liveActivityForItem(item) {
  if (!item) return null
  switch (item.type) {
    case 'reasoning':
      return { label: 'Thinking' }
    case 'agentMessage':
      return { label: 'Writing response' }
    case 'commandExecution':
      return { label: 'Running command', detail: compactDetail(item.command) }
    case 'fileChange':
      return { label: 'Editing files' }
    case 'mcpToolCall':
      return { label: 'Using MCP tool', detail: [item.server, item.tool].filter(Boolean).join('.') || null }
    case 'dynamicToolCall':
      return { label: 'Using tool', detail: [item.namespace, item.tool].filter(Boolean).join('/') || null }
    case 'webSearch':
      return { label: 'Searching the web', detail: compactDetail(item.query) }
    case 'toolUse':
      // Claude's stream-json dialect: one item type for every tool, named by
      // the block rather than by a codex-style typed item.
      return { label: 'Using tool', detail: compactDetail(item.name) }
    case 'plan':
      return { label: 'Planning' }
    default:
      return { label: 'Working' }
  }
}
