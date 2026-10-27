import { renameThread, clearThreadName } from '../explorer.svelte.js'

// One inline name editor, shared by the row cell and the detail heading. Flow
// (Slice E): begin() opens an input prefilled with the current name; Enter
// commits, Esc and blur cancel (never commit on blur); an empty/whitespace
// commit clears (DELETE). The API call is awaited — no optimism — and the input
// stays disabled while it is in flight. On error the message stays inline and
// the input stays open for correction. The dataset itself is refreshed by the
// SSE catalog push after a successful write.
export function createNameEditor() {
  const state = $state({
    editing: false, // input is shown
    saving: false, // API call in flight → input disabled, cancel suppressed
    value: '', // current input text
    error: null, // inline error message; keeps the input open
    id: null, // thread id being edited
  })

  function begin(id, currentName) {
    if (state.saving) return
    state.id = id
    state.value = currentName ?? ''
    state.error = null
    state.editing = true
  }

  // Esc/blur cancel. An in-flight commit is not cancellable — the write is
  // already going out and its result governs the final state.
  function cancel() {
    if (state.saving) return
    state.editing = false
    state.error = null
    state.value = ''
    state.id = null
  }

  async function commit() {
    if (!state.editing || state.saving) return
    const id = state.id
    const trimmed = state.value.trim()
    state.saving = true
    state.error = null
    try {
      if (trimmed === '') await clearThreadName(id)
      else await renameThread(id, trimmed)
      state.saving = false
      state.editing = false
      state.value = ''
      state.id = null
    } catch (e) {
      // Keep the input open (editing stays true) so the operator can fix it.
      state.saving = false
      state.error = e?.message ?? String(e)
    }
  }

  return { state, begin, cancel, commit }
}
