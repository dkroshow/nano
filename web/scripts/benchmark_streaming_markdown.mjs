import { performance } from 'node:perf_hooks'
import { JSDOM } from 'jsdom'
import { messageBodyHtml } from '../src/lib/transcript/message_body.js'
import { createStreamingMarkdownSurface } from '../src/lib/transcript/streaming_markdown.js'

const TARGET_BYTES = 100 * 1024
const SAMPLE_COUNT = 120
const CPU_MULTIPLIER = Number(process.argv.find((arg) => arg.startsWith('--cpu-multiplier='))?.split('=')[1] ?? 6)
const APPEND_LINE = '\nconst nextValue = values.length + 1;'

function codeBlock(bytes = TARGET_BYTES) {
  const line = 'const value = "streaming markdown benchmark";\n'
  return `\`\`\`js\n${line.repeat(Math.ceil(bytes / line.length)).slice(0, bytes)}`
}

function percentile(values, ratio) {
  const sorted = [...values].sort((left, right) => left - right)
  return sorted[Math.min(sorted.length - 1, Math.floor(sorted.length * ratio))]
}

function summarize(name, samples) {
  const adjusted = samples.map((sample) => sample * CPU_MULTIPLIER)
  return {
    name,
    samples: adjusted.length,
    cpuMultiplier: CPU_MULTIPLIER,
    p50Ms: Number(percentile(adjusted, 0.5).toFixed(2)),
    p95Ms: Number(percentile(adjusted, 0.95).toFixed(2)),
    maxMs: Number(Math.max(...adjusted).toFixed(2)),
    misses20Hz: adjusted.filter((sample) => sample > 50).length,
    misses30Hz: adjusted.filter((sample) => sample > 1000 / 30).length,
  }
}

function measureWholeBody(document, initial) {
  const root = document.createElement('div')
  const samples = []
  let source = initial
  for (let index = 0; index < SAMPLE_COUNT; index += 1) {
    source += APPEND_LINE
    const started = performance.now()
    root.innerHTML = messageBodyHtml('agent', source)
    samples.push(performance.now() - started)
  }
  return summarize('whole-body replacement', samples)
}

function measureTrailingBlock(document, initial) {
  const root = document.createElement('div')
  const surface = createStreamingMarkdownSurface(root)
  surface.replace(initial, { incremental: true })
  const samples = []
  for (let index = 0; index < SAMPLE_COUNT; index += 1) {
    const started = performance.now()
    surface.append(APPEND_LINE)
    samples.push(performance.now() - started)
  }
  return summarize('trailing-block update', samples)
}

const dom = new JSDOM('<!doctype html><body></body>')
const initial = codeBlock()
const results = {
  workload: {
    initialBytes: new TextEncoder().encode(initial).length,
    samples: SAMPLE_COUNT,
    appendBytes: new TextEncoder().encode(APPEND_LINE).length,
    frameBudgetsMs: { hz20: 50, hz30: Number((1000 / 30).toFixed(2)) },
    note: 'The multiplier is a deterministic CPU-throttling proxy; Chrome DevTools measurements should use its native 6x throttle.',
  },
  results: [
    measureWholeBody(dom.window.document, initial),
    measureTrailingBlock(dom.window.document, initial),
  ],
}

console.log(JSON.stringify(results, null, 2))
