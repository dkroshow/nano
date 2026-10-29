import { esc, md } from './markdown.js'

export function messageBodyHtml(kind, text) {
  return kind === 'agent' ? md(text || '') : esc(text)
}
