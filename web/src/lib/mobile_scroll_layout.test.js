import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'

const css = readFileSync(
  fileURLToPath(new URL('../styles/nano.css', import.meta.url)),
  'utf8',
)

function ruleBody(selector) {
  const escaped = selector.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
  // Anchor at a rule start so `.body {` does not steal the `body {` page rule.
  return css.match(new RegExp(`(?:^|\\n)${escaped}\\s*\\{([^}]*)\\}`, 's'))?.[1] ?? ''
}

describe('mobile conversation scroll chain', () => {
  it('allows the conversation column to shrink so the transcript scrolls above the composer', () => {
    // jsdom does not calculate flex layout. Guard the exact property that keeps
    // a long transcript from making this flex child taller than the viewport.
    expect(ruleBody('.pane-left')).toMatch(/min-height:\s*0\b/)
  })

  it('keeps conversation scrolling vertical so a wide table is not a page pan', () => {
    const convo = ruleBody('.convo')
    expect(convo).toMatch(/min-width:\s*0\b/)
    expect(convo).toMatch(/overflow-x:\s*clip\b/)
    expect(convo).toMatch(/overflow-y:\s*auto\b/)
    expect(convo).not.toMatch(/touch-action:\s*pan-y\b/)
  })

  it('scrolls a Markdown table inside the message wrap, not the page', () => {
    const wrap = ruleBody('.md-body .md-table-wrap')
    expect(wrap).toMatch(/overflow-x:\s*auto\b/)
    expect(wrap).toMatch(/overflow-y:\s*hidden\b/)
    expect(wrap).toMatch(/overscroll-behavior-x:\s*contain\b/)
    expect(wrap).toMatch(/max-width:\s*100%/)
  })

  it('contains box-drawing tables the same way as pipe tables', () => {
    const wrap = ruleBody('.md-body .box-table-wrap')
    expect(wrap).toMatch(/overflow-x:\s*auto\b/)
    expect(wrap).toMatch(/overflow-y:\s*hidden\b/)
    expect(wrap).toMatch(/overscroll-behavior-x:\s*contain\b/)
  })

  it('keeps the Nano page from becoming a horizontal scroller', () => {
    expect(ruleBody('body')).toMatch(/overflow:\s*hidden\b/)
  })
})
