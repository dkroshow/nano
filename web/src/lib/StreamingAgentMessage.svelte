<script>
  import { createTextStreamSmoother } from './text_stream_smoother.js'
  import { createStreamingMarkdownSurface } from './streaming_markdown.js'
  import { fileReferenceLinks } from './file_references.js'
  import { recordBrowserDiagnostic } from '../browser_diagnostics.js'

  let {
    text = '',
    smooth = false,
    complete = false,
    provider = '',
    animateInitial = false,
    diagnosticContext = {},
    onOpen = null,
    onPresentationUpdate = null,
  } = $props()

  let smoother = null
  let pageVisible = true
  let reducedMotion = false

  function shouldAnimate() {
    return smooth && pageVisible && !reducedMotion
  }

  function sync() {
    smoother?.sync({ text, animate: shouldAnimate(), complete, animateInitial })
  }

  function streamSurface(node) {
    const surface = createStreamingMarkdownSurface(node)
    const media = typeof matchMedia === 'function'
      ? matchMedia('(prefers-reduced-motion: reduce)')
      : null
    reducedMotion = Boolean(media?.matches)
    pageVisible = typeof document === 'undefined' || document.visibilityState !== 'hidden'

    smoother = createTextStreamSmoother({
      provider,
      onReplace(next, detail) {
        surface.replace(next, { incremental: detail.reason === 'initial' && detail.animate })
        onPresentationUpdate?.()
      },
      onAppend(delta) {
        surface.append(delta)
        onPresentationUpdate?.()
      },
      onSettled(finalText, metrics) {
        surface.finish(finalText)
        onPresentationUpdate?.()
        recordBrowserDiagnostic('stream.smoothing', {
          ...diagnosticContext,
          provider: metrics.provider || provider,
          arrivalRateCps: metrics.arrivalRateCps,
          batchIntervalMs: metrics.batchIntervalMs,
          batchSize: metrics.batchSize,
          batchCount: metrics.batches,
          maxBacklog: metrics.maxBacklog,
          publishCount: metrics.publishes,
          durationMs: metrics.durationMs,
        })
      },
    })
    sync()

    const handleVisibility = () => {
      pageVisible = document.visibilityState !== 'hidden'
      sync()
    }
    const handleMotion = (event) => {
      reducedMotion = Boolean(event.matches)
      sync()
    }
    document.addEventListener('visibilitychange', handleVisibility)
    media?.addEventListener?.('change', handleMotion)

    return {
      destroy() {
        smoother?.destroy()
        smoother = null
        document.removeEventListener('visibilitychange', handleVisibility)
        media?.removeEventListener?.('change', handleMotion)
      },
    }
  }

  // A parent update can insert the next transcript row in the same flush. Snap
  // a no-longer-tail message before that DOM update so presentation lag never
  // makes a newer row appear above unfinished visible text.
  $effect.pre(() => {
    void text
    void smooth
    void complete
    void animateInitial
    sync()
  })
</script>

<div
  class="bubble msg-agent-surface msg-agent-body md-body"
  data-transcript-content
  use:streamSurface
  use:fileReferenceLinks={{ onOpen }}
></div>
