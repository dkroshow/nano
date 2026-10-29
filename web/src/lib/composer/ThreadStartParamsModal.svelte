<script>
  // Thread Start Parameters reference + defaults editor. The Codex tab lists
  // all 15 thread/start params (docs/thread-start-parameters §1) and lets the
  // user set a default value + New Window visibility per param; state persists
  // to localStorage via thread_start_params.js. Claude Code tab is a stub.
  import {
    PARAMS,
    SCHEMA_VERSION,
    SCHEMA_DOC,
    ACCESS_PRESETS,
    matchAccessPreset,
    loadSettings,
    saveSettings,
  } from './thread_start_params.js'

  let { onClose } = $props()

  let tab = $state('codex')
  let settings = $state(loadSettings())
  let models = $state(null) // null = loading; [] = catalog unavailable

  async function loadModels() {
    try {
      const data = await (await fetch('/api/codex/models')).json()
      models = (data.data ?? []).filter((m) => !m.hidden)
    } catch {
      models = []
    }
  }
  loadModels()

  // Persist on any change. Reassigning `settings` (below) triggers this.
  $effect(() => {
    saveSettings(settings)
  })

  function setDefault(name, value) {
    settings = { ...settings, [name]: { ...settings[name], default: value } }
  }
  function setShow(name, checked) {
    settings = { ...settings, [name]: { ...settings[name], show: checked } }
  }

  // --- Access preset row (collapses approvalPolicy + sandbox into one). ---
  // The two params still persist independently; this row is a joint editor.
  const approvalParam = PARAMS.find((p) => p.name === 'approvalPolicy')
  const sandboxParam = PARAMS.find((p) => p.name === 'sandbox')

  const accessPreset = $derived(
    matchAccessPreset(settings.approvalPolicy.default, settings.sandbox.default),
  )
  // Display checked if either underlying show flag is set; changes normalize both.
  const accessShow = $derived(settings.approvalPolicy.show || settings.sandbox.show)

  // Manual expand toggle; a Custom pair force-expands so its state stays visible.
  let accessExpanded = $state(accessPreset === 'custom')
  const showRawRows = $derived(accessExpanded || accessPreset === 'custom')

  function setAccessPreset(value) {
    const preset = ACCESS_PRESETS.find((p) => p.value === value)
    // '' (unset) clears both; a preset writes both underlying defaults.
    const approvalPolicy = preset ? preset.approvalPolicy : ''
    const sandbox = preset ? preset.sandbox : ''
    settings = {
      ...settings,
      approvalPolicy: { ...settings.approvalPolicy, default: approvalPolicy },
      sandbox: { ...settings.sandbox, default: sandbox },
    }
  }
  function setAccessShow(checked) {
    settings = {
      ...settings,
      approvalPolicy: { ...settings.approvalPolicy, show: checked },
      sandbox: { ...settings.sandbox, show: checked },
    }
  }

  // Split inline `code` spans out of the doc text so literals render mono.
  function segments(text) {
    return text.split('`').map((part, i) => ({ code: i % 2 === 1, text: part }))
  }

  function onOverlayClick(e) {
    if (e.target === e.currentTarget) onClose?.()
  }
  function onKeydown(e) {
    if (e.key === 'Escape') onClose?.()
  }
</script>

<svelte:window onkeydown={onKeydown} />

<div class="nw-overlay" role="presentation" onclick={onOverlayClick}>
  <div class="tsp-modal" role="dialog" aria-modal="true" aria-labelledby="tsp-title">
    <button class="nw-close-x" onclick={() => onClose?.()} title="Close">×</button>
    <h3 id="tsp-title" class="tsp-title">Thread Start Parameters</h3>

    <div class="tsp-tabs">
      <button class:on={tab === 'codex'} onclick={() => (tab = 'codex')}>Codex</button>
      <button class:on={tab === 'claude'} onclick={() => (tab = 'claude')}>Claude Code</button>
    </div>

    {#if tab === 'codex'}
      <div class="tsp-note">
        Schema: {SCHEMA_VERSION} · {SCHEMA_DOC}
      </div>
      <div class="tsp-note">
        <code>summary</code>, <code>outputSchema</code>, and rich sandbox policy are
        turn-level params (doc §4), not thread/start — not listed here. <code>effort</code> is
        also turn-level but listed so a window-start default can be set.
      </div>

      <div class="tsp-table-wrap">
        <table class="tsp-table">
          <thead>
            <tr>
              <th class="tsp-col-param">Param</th>
              <th class="tsp-col-default">Default</th>
              <th
                class="tsp-col-show"
                title="Show this field in the primary New Window flow. Unchecked params live in the New Window Advanced section."
              >New Window</th>
              <th>Values / shape</th>
              <th>Notes</th>
            </tr>
          </thead>
          <tbody>
            {#each PARAMS as p (p.name)}
              {#if p.name === 'sandbox'}
                <!-- Rendered under the Access preset row, not in sequence. -->
              {:else if p.name === 'approvalPolicy'}
                <tr>
                  <td class="tsp-col-param">
                    <button
                      class="tsp-expand"
                      onclick={() => (accessExpanded = !accessExpanded)}
                      title="Show raw approvalPolicy + sandbox params"
                    >{showRawRows ? '▾' : '▸'}</button>
                    <span class="tsp-pname">Access</span>
                    <span class="tsp-ptype">preset</span>
                  </td>
                  <td class="tsp-col-default">
                    <select value={accessPreset} onchange={(e) => setAccessPreset(e.target.value)}>
                      <option value="">(unset)</option>
                      {#each ACCESS_PRESETS as ap (ap.value)}<option value={ap.value}>{ap.label}</option>{/each}
                      <option value="custom" disabled>Custom</option>
                    </select>
                  </td>
                  <td class="tsp-col-show">
                    <input type="checkbox" checked={accessShow} onchange={(e) => setAccessShow(e.target.checked)} />
                  </td>
                  <td class="tsp-muted">Read Only · Auto · Full Access</td>
                  <td class="tsp-muted">
                    Codex TUI preset names; each sets an <code>approvalPolicy</code> + <code>sandbox</code>
                    pair. Expand for raw params.
                  </td>
                </tr>
                {#if showRawRows}
                  {@render paramRow(approvalParam, true)}
                  {@render paramRow(sandboxParam, true)}
                {/if}
              {:else}
                {@render paramRow(p, false)}
              {/if}
            {/each}
          </tbody>
        </table>
      </div>
    {:else}
      <div class="tsp-placeholder">Nothing here yet.</div>
    {/if}
  </div>
</div>

{#snippet paramRow(p, raw)}
              <tr class:tsp-subrow={raw}>
                <td class="tsp-col-param">
                  <span class="tsp-pname">{p.name}</span>
                  <span class="tsp-ptype">{p.type}</span>
                </td>
                <td class="tsp-col-default">
                  {#if p.kind === 'enum'}
                    <select
                      value={settings[p.name].default}
                      onchange={(e) => setDefault(p.name, e.target.value)}
                    >
                      <option value="">(unset)</option>
                      {#each p.options as o (o.value)}<option value={o.value}>{o.label}</option>{/each}
                    </select>
                  {:else if p.kind === 'bool'}
                    <select
                      value={settings[p.name].default}
                      onchange={(e) => setDefault(p.name, e.target.value)}
                    >
                      <option value="">(unset)</option>
                      <option value="true">true</option>
                      <option value="false">false</option>
                    </select>
                  {:else if p.kind === 'model'}
                    {#if models && models.length}
                      <select
                        value={settings[p.name].default}
                        onchange={(e) => setDefault(p.name, e.target.value)}
                      >
                        <option value="">(unset)</option>
                        {#each models as m (m.id ?? m.model)}<option value={m.model}>{m.displayName ?? m.model}</option>{/each}
                      </select>
                    {:else}
                      <input
                        class="tsp-input mono"
                        value={settings[p.name].default}
                        oninput={(e) => setDefault(p.name, e.target.value)}
                        placeholder={p.placeholder}
                      />
                    {/if}
                  {:else if p.kind === 'textarea'}
                    <textarea
                      class="tsp-textarea"
                      rows="2"
                      value={settings[p.name].default}
                      oninput={(e) => setDefault(p.name, e.target.value)}
                      placeholder={p.placeholder}
                    ></textarea>
                  {:else}
                    <input
                      class="tsp-input mono"
                      value={settings[p.name].default}
                      oninput={(e) => setDefault(p.name, e.target.value)}
                      placeholder={p.placeholder}
                    />
                  {/if}
                </td>
                <td class="tsp-col-show">
                  <input
                    type="checkbox"
                    checked={settings[p.name].show}
                    onchange={(e) => setShow(p.name, e.target.checked)}
                  />
                </td>
                <td class="tsp-muted">
                  {#each segments(p.values) as seg}{#if seg.code}<code>{seg.text}</code>{:else}{seg.text}{/if}{/each}
                </td>
                <td class="tsp-muted">
                  {#each segments(p.notes) as seg}{#if seg.code}<code>{seg.text}</code>{:else}{seg.text}{/if}{/each}
                </td>
              </tr>
{/snippet}
