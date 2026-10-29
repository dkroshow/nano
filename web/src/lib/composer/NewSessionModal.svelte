<script>
  import { untrack } from 'svelte'
  import { app, spawnSession } from '../../app.svelte.js'
  import { PARAMS, loadSettings } from './thread_start_params.js'
  import { CODEX_PERMISSION_PROFILES, CUSTOM_PROFILE, pairForProfile, profileForPair } from './codex_permissions.js'
  import { cwdSuggestions } from './cwd_suggestions.js'
  import { explorer } from '../../explorer.svelte.js'
  import { loadCodexModels } from './codex_models.js'
  import { CLAUDE_START_MODELS } from './claude_models.js'
  import { loadGrokModels } from './grok_models.js'
  import { trapTab } from '../shared/modal_focus.js'

  let { onClose } = $props()

  // Dialog focus management: focus the first form control on open, keep Tab
  // cycling inside the card (trapTab via the window keydown below), restore
  // focus to the opener on close.
  let cardEl = $state(null)
  const previouslyFocused = document.activeElement
  $effect(() => {
    cardEl?.querySelector('.nw-input, input[type="radio"]')?.focus()
    return () => {
      if (previouslyFocused instanceof HTMLElement) previouslyFocused.focus()
    }
  })

  // Saved thread/start defaults + New Window visibility, read once on open.
  // Per param: `default` pre-fills form state, `show` decides primary vs
  // Advanced placement. Empty default = unset → omitted from the payload.
  const settings = loadSettings()
  const savedDefault = (name) => settings[name]?.default ?? ''
  // cwd is the shared top-of-modal input (also used by Claude) and effort is a
  // turn-level override rendered alongside the model picker — neither is
  // placed by a show flag, so both are excluded from the codex param loop.
  const CODEX_PARAMS = PARAMS.filter((p) => p.name !== 'cwd' && p.name !== 'effort')

  const CLAUDE_EFFORTS = [
    { value: 'low', label: 'Low' },
    { value: 'medium', label: 'Medium' },
    { value: 'high', label: 'High' },
    { value: 'xhigh', label: 'xHigh' },
    { value: 'max', label: 'Max' },
  ]
  const PERMISSION_MODES = [
    { value: 'acceptEdits', label: 'Accept edits' },
    { value: 'plan', label: 'Plan' },
    { value: 'auto', label: 'Auto' },
    { value: 'dontAsk', label: "Don't ask" },
    { value: 'bypassPermissions', label: 'Bypass permissions' },
  ]
  // The sandbox/approval vocabulary lives on in the wire and in stored
  // defaults; the dialog's one Permissions dial maps onto it
  // (codex_permissions.js).
  const PERSONALITIES = [
    { value: '', label: 'Default' },
    { value: 'pragmatic', label: 'Pragmatic' },
    { value: 'friendly', label: 'Friendly' },
    { value: 'none', label: 'None' },
  ]
  const REVIEWERS = [
    { value: '', label: 'Default (user)' },
    { value: 'user', label: 'User' },
    { value: 'auto_review', label: 'Auto review (subagent)' },
  ]
  let provider = $state('codex')
  let cwd = $state(savedDefault('cwd'))
  // Optional session name, written to the nano-owned names store once the
  // native thread id binds (user_assigned, same as a rename). Never a default:
  // a name is per-session by nature.
  let sessionName = $state('')

  // Working-directory combobox: recent directories from the catalog window,
  // shown on focus and filtered as the operator types (cwd_suggestions.js) —
  // both interaction styles the operator weighed, in one control. Free text stays valid.
  let cwdListOpen = $state(false)
  let cwdHighlight = $state(-1)
  const cwdOptions = $derived(
    cwdListOpen ? cwdSuggestions({ dataset: explorer.dataset, sessions: app.sessions, query: cwd }) : [],
  )
  function chooseCwd(path) {
    cwd = path
    cwdListOpen = false
    cwdHighlight = -1
  }
  function onCwdKeydown(e) {
    if (!cwdListOpen || cwdOptions.length === 0) return
    if (e.key === 'ArrowDown') {
      e.preventDefault()
      cwdHighlight = (cwdHighlight + 1) % cwdOptions.length
    } else if (e.key === 'ArrowUp') {
      e.preventDefault()
      cwdHighlight = (cwdHighlight - 1 + cwdOptions.length) % cwdOptions.length
    } else if (e.key === 'Enter' && cwdHighlight >= 0) {
      e.preventDefault()
      chooseCwd(cwdOptions[cwdHighlight].path)
    } else if (e.key === 'Escape') {
      e.stopPropagation()
      cwdListOpen = false
      cwdHighlight = -1
    }
  }
  let busy = $state(false)
  let error = $state(null)

  // codex params
  let codexMode = $state('start')
  let resumeThreadId = $state('')
  let models = $state(null) // null = loading; [] = catalog unavailable
  let codexModel = $state(savedDefault('model')) // free-text fallback seed; catalog load overrides
  let codexEffort = $state(savedDefault('effort')) // free-text fallback seed; catalog load overrides
  // Standard is an explicit app-server value. Empty means "inherit config",
  // which can itself be Priority and therefore must not be labelled Standard.
  let codexTier = $state('default')
  let sandbox = $state(savedDefault('sandbox') || app.defaults.sandbox || 'read-only')
  let approval = $state(savedDefault('approvalPolicy') || app.defaults.approval || 'never')
  // The dial's position is derived from the pair, so a stored default outside
  // the profile table surfaces as Custom instead of being rewritten.
  const permissionProfile = $derived(profileForPair(sandbox, approval))
  function choosePermissions(id) {
    const pair = pairForProfile(id)
    if (!pair) return // Custom: keep the stored pair as it is
    sandbox = pair.sandbox
    approval = pair.approval
  }

  // codex advanced params (thread/start; docs/thread-start-parameters §1/§7)
  let personality = $state(savedDefault('personality'))
  let approvalsReviewer = $state(savedDefault('approvalsReviewer'))
  let ephemeral = $state(savedDefault('ephemeral') === 'true')
  let baseInstructions = $state(savedDefault('baseInstructions'))
  let developerInstructions = $state(savedDefault('developerInstructions'))
  let modelProvider = $state(savedDefault('modelProvider'))
  let config = $state(savedDefault('config'))
  let threadSource = $state(savedDefault('threadSource'))
  let sessionStartSource = $state(savedDefault('sessionStartSource'))
  let serviceName = $state(savedDefault('serviceName'))

  // claude params
  let claudeModel = $state('opus')
  let claudeEffort = $state('medium')
  let permissionMode = $state('bypassPermissions')
  let allowedTools = $state('')

  // grok params — start mode only. Historical/resume for Grok is Explorer's
  // resume policy (thread_resume.svelte.js), not New Window. Sandbox remains
  // fixed off by the shared leader. Grok 1.0.3 exposes auto and bypass as
  // per-session ACP options; empty inherits the normal Grok configuration.
  let grokModels = $state(null) // null = loading; [] = catalog unavailable
  let grokModel = $state('')
  let grokEffort = $state('')
  let grokPermissionMode = $state('')

  const selectedModel = $derived(models?.find((m) => m.model === codexModel) ?? null)
  const tiers = $derived(selectedModel?.serviceTiers ?? [])

  // Show flag decides primary vs Advanced placement for each codex param.
  const primaryParams = $derived(CODEX_PARAMS.filter((p) => settings[p.name].show))
  const advancedParams = $derived(CODEX_PARAMS.filter((p) => !settings[p.name].show))

  // config is a JSON object escape hatch: only include it when it parses to a
  // non-null object. `configError` surfaces inline and blocks submit.
  const configTrimmed = $derived(config.trim())
  const configParsed = $derived.by(() => {
    if (!configTrimmed) return { ok: true, value: undefined }
    try {
      const value = JSON.parse(configTrimmed)
      if (value === null || typeof value !== 'object' || Array.isArray(value)) return { ok: false, value: undefined }
      return { ok: true, value }
    } catch {
      return { ok: false, value: undefined }
    }
  })
  const configError = $derived(configTrimmed && !configParsed.ok ? 'Config must be a JSON object.' : null)
  const claudeAllowedToolsPlaceholder = $derived(
    permissionMode === 'bypassPermissions'
      ? 'all tools'
      : app.defaults.allowedTools
        ? `server default: ${app.defaults.allowedTools}`
        : 'all tools'
  )
  const isCodexResume = $derived(provider === 'codex' && codexMode === 'resume')
  const canSubmit = $derived(
    !busy && !(isCodexResume && !resumeThreadId.trim()) && !(provider === 'codex' && codexMode === 'start' && configError)
  )
  const submitLabel = $derived(isCodexResume ? 'Resume' : 'Create')
  const busyLabel = $derived(isCodexResume ? 'Resuming…' : 'Creating…')
  // Why the Create button is disabled — same conditions as canSubmit.
  const submitBlocker = $derived.by(() => {
    if (busy) return null
    if (isCodexResume && !resumeThreadId.trim()) return 'Enter a thread ID to resume.'
    if (provider === 'codex' && codexMode === 'start' && configError) return 'Fix the config JSON to continue.'
    return null
  })

  async function loadModels() {
    try {
      models = await loadCodexModels(true)
    } catch (err) {
      error = err.message ?? String(err)
      return
    }
    // A saved model default wins when present in the catalog; otherwise fall
    // back to the pinned flagship, then the advertised default, then first.
    const savedModel = savedDefault('model')
    const def =
      (savedModel && models.find((m) => m.model === savedModel)) ??
      models.find((m) => m.model === 'gpt-5.6-sol') ??
      models.find((m) => m.isDefault) ??
      models[0]
    if (def) {
      selectModel(def.model)
      // selectModel resets effort/tier to the model defaults; re-apply
      // saved defaults when that model supports them.
      const savedEffort = savedDefault('effort')
      if (savedEffort && (def.supportedReasoningEfforts ?? []).some((e) => e.reasoningEffort === savedEffort)) codexEffort = savedEffort
      const savedTier = savedDefault('serviceTier')
      if (savedTier === 'default' || (savedTier && (def.serviceTiers ?? []).some((t) => t.id === savedTier))) codexTier = savedTier
    }
    // Catalog seeding is programmatic, not a user edit — refresh the
    // dirty-guard baseline so an untouched form still closes on first Esc.
    baselinePayload = JSON.stringify(payload)
  }
  loadModels()

  // Model change resets effort/tier to that model's advertised defaults.
  function selectModel(value) {
    codexModel = value
    const m = models?.find((mm) => mm.model === value)
    codexEffort = m?.defaultReasoningEffort ?? ''
    codexTier = m?.defaultServiceTier ?? 'default'
  }

  async function loadGrokModelCatalog() {
    grokModels = await loadGrokModels()
    const def = grokModels.find((m) => m.isDefault) ?? grokModels[0]
    if (def) selectGrokModel(def.model)
    baselinePayload = JSON.stringify(payload)
  }
  loadGrokModelCatalog()

  const selectedGrokModel = $derived(grokModels?.find((m) => m.model === grokModel) ?? null)
  function selectGrokModel(value) {
    grokModel = value
    const m = grokModels?.find((mm) => mm.model === value)
    grokEffort = m?.defaultReasoningEffort ?? ''
  }

  // The spawn request, derived once — submit posts it and the wire pane
  // previews it, so both always describe the same spawn.
  const payload = $derived(
    provider === 'codex'
      ? codexMode === 'resume'
        ? {
            provider,
            mode: 'resume',
            threadId: resumeThreadId.trim() || undefined,
            name: sessionName.trim() || undefined,
          }
        : {
          provider,
          mode: 'start',
          name: sessionName.trim() || undefined,
          cwd: cwd.trim() || undefined,
          model: codexModel || undefined,
          effort: codexEffort || undefined,
          serviceTier: codexTier,
          sandbox,
          approval,
          personality: personality || undefined,
          approvalsReviewer: approvalsReviewer || undefined,
          ephemeral: ephemeral || undefined,
          baseInstructions: baseInstructions.trim() || undefined,
          developerInstructions: developerInstructions.trim() || undefined,
          modelProvider: modelProvider.trim() || undefined,
          config: configParsed.value,
          threadSource: threadSource.trim() || undefined,
          sessionStartSource: sessionStartSource || undefined,
          serviceName: serviceName.trim() || undefined,
        }
      : provider === 'grok'
        ? {
            provider,
            mode: 'start',
            name: sessionName.trim() || undefined,
            cwd: cwd.trim() || undefined,
            model: grokModel || undefined,
            effort: grokEffort || undefined,
            permissionMode: grokPermissionMode || undefined,
          }
        : {
          provider,
          mode: 'start',
          name: sessionName.trim() || undefined,
          cwd: cwd.trim() || undefined,
          model: claudeModel,
          effort: claudeEffort,
          permissionMode,
          allowedTools: allowedTools.trim() || undefined,
        }
  )

  // Dirty-form close guard: baseline the payload at open (untracked snapshot;
  // loadModels re-baselines after its programmatic seeding). Esc/backdrop on a
  // modified form warns once; a second attempt within 4s discards. The ×
  // button always closes immediately (explicit intent).
  const DISCARD_CONFIRM_MS = 4000
  let baselinePayload = untrack(() => JSON.stringify(payload))
  let confirmDiscard = $state(false)
  let discardTimer = null
  function requestClose() {
    if (busy) return
    if (confirmDiscard || JSON.stringify(payload) === baselinePayload) {
      onClose?.()
      return
    }
    confirmDiscard = true
    clearTimeout(discardTimer)
    discardTimer = setTimeout(() => (confirmDiscard = false), DISCARD_CONFIRM_MS)
  }
  // One live-region hint slot under Create: the discard confirmation outranks
  // the disabled-submit reason while it is pending.
  const footerHint = $derived(confirmDiscard ? 'Press Esc again to discard changes.' : submitBlocker)

  // Live wire preview: the server renders the exact protocol payload via the
  // same code path a real spawn uses (POST /api/sessions/preview).
  let preview = $state(null)
  $effect(() => {
    const body = JSON.stringify(payload)
    const ctrl = new AbortController()
    const t = setTimeout(async () => {
      try {
        const res = await fetch('/api/sessions/preview', {
          method: 'POST',
          headers: { 'content-type': 'application/json' },
          body,
          signal: ctrl.signal,
        })
        preview = res.ok ? await res.json() : null
      } catch (err) {
        if (err?.name !== 'AbortError') preview = null // stale responses are just dropped
      }
    }, 150)
    return () => {
      clearTimeout(t)
      ctrl.abort()
    }
  })

  async function submit() {
    if (!canSubmit) return
    busy = true
    error = null
    const err = await spawnSession(payload)
    busy = false
    if (err) error = err.message ?? err
    else {
      onClose?.()
    }
  }

  function onOverlayClick(e) {
    if (e.target === e.currentTarget) requestClose()
  }
  function onKeydown(e) {
    if (e.key === 'Escape') requestClose()
    else if (e.key === 'Tab') trapTab(cardEl, e)
  }

  // Start/Resume radio group: arrow keys move selection + focus together.
  let modeStartBtn = $state(null)
  let modeResumeBtn = $state(null)
  function onModeArrow(e) {
    if (e.key !== 'ArrowLeft' && e.key !== 'ArrowRight') return
    e.preventDefault()
    codexMode = codexMode === 'start' ? 'resume' : 'start'
    ;(codexMode === 'start' ? modeStartBtn : modeResumeBtn)?.focus()
  }
</script>

<svelte:window onkeydown={onKeydown} />

<!-- One codex thread/start control, dispatched by param name. Bespoke controls
     (model + effort, catalog service tiers, sandbox, approvals) keep their
     wire-driven behavior; the rest are plain enum/text/textarea inputs. The
     caller places each in the primary section or Advanced per its show flag. -->
{#snippet codexField(p)}
  {#if p.name === 'model'}
    {#if models === null}
      <div class="nw-loading">Loading model catalog…</div>
    {:else if models.length === 0}
      <div class="nw-model-row">
        <label for="nw-codex-model-text">Model</label>
        <input id="nw-codex-model-text" class="nw-input mono" bind:value={codexModel} placeholder="provider default" />
      </div>
    {:else}
      <div class="nw-model-row">
        <label for="nw-codex-model">Model</label>
        <select id="nw-codex-model" value={codexModel} onchange={(e) => selectModel(e.target.value)}>
          {#each models as m (m.id)}
            <option value={m.model}>{m.displayName}</option>
          {/each}
        </select>
      </div>
      <div class="nw-model-row">
        <label for="nw-codex-effort">Effort</label>
        <select id="nw-codex-effort" bind:value={codexEffort}>
          {#each selectedModel?.supportedReasoningEfforts ?? [] as e (e.reasoningEffort)}
            <option value={e.reasoningEffort} title={e.description}>{e.reasoningEffort}</option>
          {/each}
        </select>
      </div>
    {/if}
  {:else if p.name === 'serviceTier'}
    {#if tiers.length}
      <div class="nw-model-row">
        <label for="nw-codex-tier">Speed</label>
        <select id="nw-codex-tier" bind:value={codexTier}>
          <option value="default">Standard</option>
          {#each tiers as t (t.id)}
            <option value={t.id} title={t.description}>{t.name || t.id}</option>
          {/each}
        </select>
      </div>
    {/if}
  {:else if p.name === 'sandbox'}
    <!-- One permissions dial (the operator, 2026-07-31), speaking the profile names
         codex itself advertises. The wire still takes the sandbox × approval
         pair — the mapping lives in codex_permissions.js and the preview pane
         keeps showing the real parameters. Custom appears only when a stored
         default is a pair the profiles don't cover, and preserves it. -->
    <div class="nw-model-row">
      <label for="nw-permissions">Permissions</label>
      <select id="nw-permissions" value={permissionProfile} onchange={(e) => choosePermissions(e.target.value)}>
        {#each CODEX_PERMISSION_PROFILES as prof (prof.id)}
          <option value={prof.id}>{prof.label}</option>
        {/each}
        {#if permissionProfile === CUSTOM_PROFILE}
          <option value={CUSTOM_PROFILE}>Custom ({sandbox} · {approval})</option>
        {/if}
      </select>
    </div>
  {:else if p.name === 'approvalPolicy'}
    <!-- Folded into the Permissions dial above. -->
  {:else if p.name === 'personality'}
    {#if selectedModel?.supportsPersonality !== false}
      <div class="nw-model-row">
        <label for="nw-personality">Personality</label>
        <select id="nw-personality" bind:value={personality}>
          {#each PERSONALITIES as pp (pp.value)}<option value={pp.value}>{pp.label}</option>{/each}
        </select>
      </div>
    {/if}
  {:else if p.name === 'approvalsReviewer'}
    <div class="nw-model-row">
      <label for="nw-reviewer">Approvals reviewer</label>
      <select id="nw-reviewer" bind:value={approvalsReviewer}>
        {#each REVIEWERS as r (r.value)}<option value={r.value}>{r.label}</option>{/each}
      </select>
    </div>
  {:else if p.name === 'ephemeral'}
    <label class="nw-inline-check">
      <input type="checkbox" bind:checked={ephemeral} />
      Ephemeral <span class="nw-hint">no rollout on disk, cannot resume</span>
    </label>
  {:else if p.name === 'baseInstructions'}
    <div class="nw-model-row">
      <label for="nw-base-instr">Base instructions <span class="nw-hint">replaces built-in system prompt layer</span></label>
      <textarea id="nw-base-instr" class="nw-textarea" rows="3" bind:value={baseInstructions} placeholder="built-in default"></textarea>
    </div>
  {:else if p.name === 'developerInstructions'}
    <div class="nw-model-row">
      <label for="nw-dev-instr">Developer instructions</label>
      <textarea id="nw-dev-instr" class="nw-textarea" rows="3" bind:value={developerInstructions} placeholder="none"></textarea>
    </div>
  {:else if p.name === 'modelProvider'}
    <div class="nw-model-row">
      <label for="nw-model-provider">Model provider</label>
      <input id="nw-model-provider" class="nw-input mono" bind:value={modelProvider} placeholder={p.placeholder} />
    </div>
  {:else if p.name === 'config'}
    <div class="nw-model-row">
      <label for="nw-config">Config <span class="nw-hint">per-thread config.toml overrides (JSON object)</span></label>
      <textarea
        id="nw-config"
        class="nw-textarea mono"
        rows="3"
        bind:value={config}
        placeholder={p.placeholder}
        aria-invalid={!!configError}
        aria-describedby={configError ? 'nw-config-error' : undefined}
      ></textarea>
      {#if configError}<div class="nw-field-error" id="nw-config-error" role="alert">{configError}</div>{/if}
    </div>
  {:else if p.name === 'threadSource'}
    <div class="nw-model-row">
      <label for="nw-thread-source">Thread source</label>
      <input id="nw-thread-source" class="nw-input mono" bind:value={threadSource} placeholder={p.placeholder} />
    </div>
  {:else if p.name === 'sessionStartSource'}
    <div class="nw-model-row">
      <label for="nw-session-start">Session start source</label>
      <select id="nw-session-start" bind:value={sessionStartSource}>
        <option value="">Default</option>
        {#each p.options as o (o.value)}<option value={o.value}>{o.label}</option>{/each}
      </select>
    </div>
  {:else if p.name === 'serviceName'}
    <div class="nw-model-row">
      <label for="nw-service-name">Service name</label>
      <input id="nw-service-name" class="nw-input mono" bind:value={serviceName} placeholder={p.placeholder} />
    </div>
  {/if}
{/snippet}

<!-- role=presentation: the backdrop is a pointer-only affordance (Esc and the
     × cover keyboard/AT), matching the Thread Explorer detail overlay. -->
<div class="nw-overlay" role="presentation" onclick={onOverlayClick}>
  <div class="nw-modal" role="dialog" aria-modal="true" aria-labelledby="nw-title" bind:this={cardEl}>
    <button class="nw-close-x" disabled={busy} onclick={() => onClose?.()} title="Close">×</button>
    <h3 id="nw-title">New Window</h3>

    <div class="nw-body">
    <div class="nw-form">
    <div class="nw-model-row">
      <label for="nw-name">Name</label>
      <input
        id="nw-name"
        class="nw-input"
        bind:value={sessionName}
        placeholder="optional — names the thread everywhere"
        autocomplete="off"
      />
    </div>

    {#if !isCodexResume}
      <div class="nw-model-row">
        <label for="nw-cwd">Working directory</label>
        <div class="nw-cwd-combo">
          <input
            id="nw-cwd"
            class="nw-input mono"
            bind:value={cwd}
            placeholder={app.defaults.cwd || 'server default'}
            autocomplete="off"
            onfocus={() => (cwdListOpen = true)}
            oninput={() => { cwdListOpen = true; cwdHighlight = -1 }}
            onblur={() => setTimeout(() => (cwdListOpen = false), 120)}
            onkeydown={onCwdKeydown}
          />
          {#if cwdListOpen && cwdOptions.length}
            <div class="nw-cwd-list" role="listbox">
              {#each cwdOptions as opt, i (opt.path)}
                <button
                  type="button"
                  role="option"
                  aria-selected={i === cwdHighlight}
                  class="nw-cwd-option"
                  class:active={i === cwdHighlight}
                  title={opt.path}
                  onmousedown={(e) => { e.preventDefault(); chooseCwd(opt.path) }}
                >{opt.label}</button>
              {/each}
            </div>
          {/if}
        </div>
      </div>
    {/if}

    <div class="nw-check-row">
      <input type="radio" id="nw-provider-codex" name="provider" value="codex" bind:group={provider} />
      <label for="nw-provider-codex">Codex</label>
    </div>
    {#if provider === 'codex'}
      <div class="nw-params">
        <div class="nw-mode-toggle" role="radiogroup" aria-label="codex session mode">
          <button
            role="radio"
            aria-checked={codexMode === 'start'}
            tabindex={codexMode === 'start' ? 0 : -1}
            class:on={codexMode === 'start'}
            bind:this={modeStartBtn}
            onclick={() => (codexMode = 'start')}
            onkeydown={onModeArrow}
          >Start</button>
          <button
            role="radio"
            aria-checked={codexMode === 'resume'}
            tabindex={codexMode === 'resume' ? 0 : -1}
            class:on={codexMode === 'resume'}
            bind:this={modeResumeBtn}
            onclick={() => (codexMode = 'resume')}
            onkeydown={onModeArrow}
          >Resume</button>
        </div>

        {#if codexMode === 'resume'}
          <div class="nw-model-row">
            <label for="nw-resume-thread">Thread ID</label>
            <input id="nw-resume-thread" class="nw-input mono" bind:value={resumeThreadId} placeholder="019f…" autocomplete="off" />
          </div>
        {:else}
          <!-- Primary params: those flagged "show in New Window" in settings.
               Effort rides along with model (turn-level, model-scoped). -->
          {#each primaryParams as p (p.name)}{@render codexField(p)}{/each}
          {#if advancedParams.length}
            <details class="nw-advanced">
              <summary>Advanced</summary>
              {#each advancedParams as p (p.name)}{@render codexField(p)}{/each}
            </details>
          {/if}
        {/if}
      </div>
    {/if}

    <div class="nw-check-row">
      <input type="radio" id="nw-provider-claude" name="provider" value="claude" bind:group={provider} />
      <label for="nw-provider-claude">Claude Code</label>
    </div>
    {#if provider === 'claude'}
      <div class="nw-params">
        <div class="nw-model-row">
          <label for="nw-claude-model">Model</label>
          <select id="nw-claude-model" bind:value={claudeModel}>
            {#each CLAUDE_START_MODELS as m (m.value)}<option value={m.value}>{m.label}</option>{/each}
          </select>
        </div>
        <div class="nw-model-row">
          <label for="nw-claude-effort">Effort</label>
          <select id="nw-claude-effort" bind:value={claudeEffort}>
            {#each CLAUDE_EFFORTS as e (e.value)}<option value={e.value}>{e.label}</option>{/each}
          </select>
        </div>
        <div class="nw-model-row">
          <label for="nw-permission-mode">Permissions</label>
          <select id="nw-permission-mode" bind:value={permissionMode}>
            {#each PERMISSION_MODES as p (p.value)}<option value={p.value}>{p.label}</option>{/each}
          </select>
        </div>
        <div class="nw-model-row">
          <label for="nw-allowed-tools">Allowed tools</label>
          <input id="nw-allowed-tools" class="nw-input mono" bind:value={allowedTools} placeholder={claudeAllowedToolsPlaceholder} />
        </div>
      </div>
    {/if}

    <div class="nw-check-row">
      <input type="radio" id="nw-provider-grok" name="provider" value="grok" bind:group={provider} />
      <label for="nw-provider-grok">Grok Build <span class="n">experimental</span></label>
    </div>
    {#if provider === 'grok'}
      <div class="nw-params">
        {#if grokModels === null}
          <div class="nw-loading">Loading model catalog…</div>
        {:else if grokModels.length === 0}
          <div class="nw-model-row">
            <label for="nw-grok-model-text">Model</label>
            <input id="nw-grok-model-text" class="nw-input mono" bind:value={grokModel} placeholder="inherit" />
          </div>
          <div class="nw-model-row">
            <label for="nw-grok-effort-text">Effort</label>
            <input id="nw-grok-effort-text" class="nw-input mono" bind:value={grokEffort} placeholder="inherit" />
          </div>
        {:else}
          <div class="nw-model-row">
            <label for="nw-grok-model">Model</label>
            <select id="nw-grok-model" value={grokModel} onchange={(e) => selectGrokModel(e.target.value)}>
              {#each grokModels as m (m.model)}
                <option value={m.model}>{m.displayName ?? m.model}</option>
              {/each}
            </select>
          </div>
          <div class="nw-model-row">
            <label for="nw-grok-effort">Effort</label>
            <select id="nw-grok-effort" bind:value={grokEffort}>
              <option value="">inherit</option>
              {#each selectedGrokModel?.supportedReasoningEfforts ?? [] as e (e.reasoningEffort)}
                <option value={e.reasoningEffort} title={e.label}>{e.reasoningEffort}</option>
              {/each}
            </select>
          </div>
        {/if}
        <div class="nw-model-row">
          <label for="nw-grok-permission-mode">Permissions</label>
          <select id="nw-grok-permission-mode" bind:value={grokPermissionMode}>
            <option value="">Grok default</option>
            <option value="auto">Auto</option>
            <option value="bypassPermissions">Bypass permissions</option>
          </select>
        </div>
        <div class="nw-hint">
          This choice applies when the Grok thread starts. Sandbox remains fixed off by the shared leader.
        </div>
      </div>
    {/if}
    </div>

    <div class="nw-wire">
      <div class="nw-wire-head">
        {#if provider === 'codex'}wire · {preview?.method ?? (codexMode === 'resume' ? 'thread/resume' : 'thread/start')}
        {:else if provider === 'claude'}wire · claude argv
        {:else}wire · grok{/if}
      </div>
      {#if !preview || preview.provider !== provider}
        <div class="nw-loading">…</div>
      {:else if preview.provider === 'codex'}
        <pre>{JSON.stringify(preview.params, null, 2)}</pre>
        {#if Object.keys(preview.turnOverrides ?? {}).length}
          <div class="nw-wire-label">+ turn/start overrides (sent with every turn)</div>
          <pre>{JSON.stringify(preview.turnOverrides, null, 2)}</pre>
        {/if}
      {:else if preview.provider === 'claude'}
        <pre>{JSON.stringify(preview.argv, null, 2)}</pre>
      {:else}
        <pre>{JSON.stringify(preview.params ?? preview.argv ?? preview, null, 2)}</pre>
      {/if}
    </div>
    </div>

    {#if error}<div class="nw-error" role="alert">{error}</div>{/if}

    <button class="nw-create-btn" disabled={!canSubmit} onclick={submit}>{busy ? busyLabel : submitLabel}</button>
    {#if footerHint}<div class="nw-submit-hint" aria-live="polite">{footerHint}</div>{/if}
  </div>
</div>
