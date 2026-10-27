<script>
  import {
    DECISION_ACCEPT,
    interactionActions,
    interactionCommand,
    interactionDetails,
    interactionKindLabel,
    interactionQuestions,
    interactionTitle,
    optionLabel,
  } from './interactions.js'

  let { interaction, onAnswer } = $props()

  let submitting = $state('')
  let errorText = $state('')
  // Selected option labels per question id — a string, or an array for a
  // multi-select question. A free-text question keeps its typed value in the
  // same map, so the answer payload is built one way.
  let picks = $state({})

  const title = $derived(interactionTitle(interaction))
  const kindLabel = $derived(interactionKindLabel(interaction))
  const details = $derived(interactionDetails(interaction))
  const command = $derived(interactionCommand(interaction))
  const questions = $derived(interactionQuestions(interaction))
  const actions = $derived(interactionActions(interaction))
  // A question with nothing chosen cannot be sent — the response shape requires
  // an answer per question, so an empty Send would be a lie about what the
  // operator decided.
  const answerable = $derived(questions.length === 0 || questions.every((q) => picked(q).length > 0))

  // picked normalizes a question's selection to the list the answer payload
  // wants, so single-select, multi-select and free text are read one way.
  function picked(q) {
    const value = picks[q.id]
    if (Array.isArray(value)) return value
    return String(value ?? '').trim() ? [String(value)] : []
  }

  function isPicked(q, label) {
    return picked(q).includes(label)
  }

  // A multi-select question toggles labels in and out of a set; every other
  // question replaces its single value.
  function choose(q, value) {
    if (!q.multiSelect) {
      picks = { ...picks, [q.id]: value }
      return
    }
    const current = picked(q)
    const next = current.includes(value) ? current.filter((v) => v !== value) : [...current, value]
    picks = { ...picks, [q.id]: next }
  }

  function answersPayload() {
    if (!questions.length) return undefined
    const out = {}
    for (const q of questions) out[q.id] = picked(q)
    return out
  }

  async function decide(decision) {
    if (submitting) return
    if (decision === DECISION_ACCEPT && !answerable) return
    submitting = decision
    errorText = ''
    try {
      // The owning store drops the card when Nano acknowledges this POST. A
      // server projection also removes it idempotently in every other tab.
      const message = await onAnswer(interaction.id, {
        decision,
        answers: decision === DECISION_ACCEPT ? answersPayload() : undefined,
      })
      if (message) errorText = message
    } finally {
      submitting = ''
    }
  }
</script>

<div class="interaction" data-kind={interaction.kind}>
  <div class="interaction-head">
    <span class="interaction-dot"></span>
    <span class="interaction-title">{title}</span>
    <span class="interaction-kind">{kindLabel}</span>
  </div>

  {#each details as line}
    <div class="interaction-detail">{line}</div>
  {/each}

  {#if command}
    <pre class="interaction-command">{command}</pre>
  {/if}

  {#each questions as q (q.id)}
    <div class="interaction-question">
      {#if q.header}<div class="interaction-question-header">{q.header}</div>{/if}
      <div class="interaction-question-text">{q.question}</div>
      {#if q.freeText}
        <input
          class="interaction-free"
          type={q.isSecret ? 'password' : 'text'}
          value={picks[q.id] ?? ''}
          oninput={(e) => choose(q, e.currentTarget.value)}
          aria-label={q.question || q.header}
        />
      {:else}
        <div
          class="interaction-options"
          role={q.multiSelect ? 'group' : 'radiogroup'}
          aria-label={q.question || q.header}
        >
          {#each q.options as option (option.label)}
            <button
              class="interaction-option"
              class:on={isPicked(q, optionLabel(option))}
              role={q.multiSelect ? 'checkbox' : 'radio'}
              aria-checked={isPicked(q, optionLabel(option))}
              onclick={() => choose(q, optionLabel(option))}
            >
              <span class="interaction-option-label">
                {optionLabel(option)}
                {#if option.recommended}<span class="interaction-recommended">Recommended</span>{/if}
              </span>
              {#if option.description}
                <span class="interaction-option-desc">{option.description}</span>
              {/if}
            </button>
          {/each}
        </div>
      {/if}
    </div>
  {/each}

  <div class="interaction-actions">
    {#each actions as action (action.decision)}
      <button
        class="interaction-btn {action.tone}"
        disabled={Boolean(submitting) || (action.decision === DECISION_ACCEPT && !answerable)}
        onclick={() => decide(action.decision)}
      >{submitting === action.decision ? '…' : action.label}</button>
    {/each}
  </div>

  {#if errorText}<div class="interaction-error">{errorText}</div>{/if}
</div>
