import { describe, expect, it } from 'vitest'
import { liveActivityForItem } from './live_turn.js'

describe('liveActivityForItem', () => {
  it('turns Codex items into concise live status', () => {
    expect(liveActivityForItem({ type: 'reasoning' })).toEqual({ label: 'Thinking' })
    expect(liveActivityForItem({ type: 'commandExecution', command: 'npm test\nwith more output' })).toEqual({
      label: 'Running command',
      detail: 'npm test',
    })
    expect(liveActivityForItem({ type: 'mcpToolCall', server: 'github', tool: 'search' })).toEqual({
      label: 'Using MCP tool',
      detail: 'github.search',
    })
  })
})
