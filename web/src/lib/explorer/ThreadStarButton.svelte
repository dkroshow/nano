<script>
  import { patchThreadStarred, setThreadStarred } from './thread_stars.js'

  let { id = '', starred = false } = $props()
  let busy = $state(false)
  let error = $state('')

  async function toggle(event) {
    event.stopPropagation()
    event.preventDefault()
    if (!id || busy) return
    const next = !starred
    busy = true
    error = ''
    patchThreadStarred({ id, starred: next })
    try {
      await setThreadStarred(id, next)
    } catch (err) {
      patchThreadStarred({ id, starred: !next })
      error = String(err?.message ?? err)
    } finally {
      busy = false
    }
  }
</script>

<button
  class="thread-star"
  class:on={starred}
  type="button"
  disabled={busy || !id}
  aria-pressed={starred}
  aria-label={starred ? 'Unstar thread' : 'Star thread'}
  title={error || (starred ? 'Unstar' : 'Star')}
  data-testid="thread-star"
  onclick={toggle}
>★</button>
