<script>
  let { resolution, recorded = null, provider = 'codex' } = $props()

  const values = $derived(resolution?.values ?? {})
  const instructions = $derived(values.instructionSources?.value ?? [])
  const configSources = $derived(values.configSources?.value ?? [])
  const skills = $derived(values.skills?.value ?? [])
  const mcpServers = $derived(values.mcpServers?.value ?? [])
  const plugins = $derived(values.plugins?.value ?? [])
  const permissions = $derived(values.permissions?.value ?? {})
  const git = $derived(values.git?.value ?? null)
  const memorySources = $derived(instructions.filter((source) => source.kind === 'auto-memory'))
  const conditionalSources = $derived(instructions.filter((source) => source.load === 'conditional'))

  function display(field, fallback = 'unknown') {
    if (!field || field.state !== 'known') return fallback
    if (typeof field.value === 'string') return field.value
    if (field.value == null) return fallback
    return JSON.stringify(field.value)
  }

  function recordedValue(key) {
    const field = recorded?.[key]
    return field?.state === 'known' ? field.value : null
  }

  function workspaceRootsValue() {
    const roots = values.workspaceRoots
    if (!roots || roots.state !== 'known') return 'unknown'
    if (!Array.isArray(roots.value)) return 'unknown'
    return roots.value.length ? roots.value.join(', ') : 'none'
  }

  const recordedDifferences = $derived.by(() => {
    if (!resolution || !recorded) return []
    const comparisons = provider === 'claude'
      ? [
          ['model', 'model', display(values.configuredModel)],
          ['effort', 'effort', display(values.configuredEffort)],
          ['permission', 'permission mode', display(values.configuredPermissionMode)],
          ['historicalBranch', 'git branch', git?.branch ?? ''],
        ]
      : [
          ['model', 'model', display(values.model)],
          ['effort', 'effort', display(values.reasoningEffort)],
          ['priority', 'service tier', display(values.serviceTier)],
          ['historicalBranch', 'git branch', git?.branch ?? ''],
        ]
    return comparisons
      .map(([key, label, current]) => ({ label, recorded: recordedValue(key), current }))
      .filter((entry) => entry.recorded && entry.current && entry.current !== 'unknown' && entry.recorded !== entry.current)
  })

  function resolvedTime(value) {
    if (!value) return ''
    return new Date(value).toLocaleString()
  }
</script>

<section class="thread-live-detail">
  <div class="xsect-head">
    Current setup
    {#if resolution?.resolvedAt}<span class="xsect-n">{resolvedTime(resolution.resolvedAt)}</span>{/if}
  </div>

  {#if !resolution}
    <div class="muted">Resolving the current {provider === 'claude' ? 'Claude' : 'Codex'} setup for this workspace…</div>
  {:else if resolution.state === 'resolving'}
    <div class="muted">Preparing the current {provider === 'claude' ? 'Claude' : 'Codex'} setup in the background…</div>
    {#each resolution.diagnostics ?? [] as diagnostic}<div class="context-note">{diagnostic}</div>{/each}
  {:else if resolution.state === 'unavailable' || resolution.state === 'failed'}
    <div class="ifile-error">Current setup {resolution.state}.</div>
    {#each resolution.diagnostics ?? [] as diagnostic}<div class="context-note">{diagnostic}</div>{/each}
  {:else}
    <div class="context-note">
      {#if provider === 'claude'}
        Reconstructed from the current workspace using Claude’s documented loading rules. Recorded thread facts are labeled separately.
      {:else}
        Resolved from the current Codex configuration and workspace.
      {/if}
      {#if resolution.stale} Refreshing newer information in the background.{/if}
    </div>

    <div class="rinput">
      <details class="sect" open>
        <summary>{provider === 'claude' ? 'Instructions & Memory' : 'Model Instructions'} <span class="n">{instructions.length} sources</span></summary>
        <div class="sect-body">
          {#each instructions as source (source.path)}
            <div class="list-row">
              <div class="name">{source.path}</div>
              <div class="desc">
                {source.scope}{source.load ? ` · ${source.load}` : ''}{source.truncated ? ' · truncated to Codex byte limit' : ''}
              </div>
            </div>
          {:else}
            <div class="muted">No instruction files were found for this workspace.</div>
          {/each}
          {#if provider === 'claude' && (memorySources.length || conditionalSources.length)}
            <div class="context-note">{memorySources.length} auto-memory entrypoint · {conditionalSources.length} path-conditional rules.</div>
          {/if}
        </div>
      </details>

      <details class="sect" open>
        <summary>Tools &amp; MCP <span class="n">{mcpServers.length} configured servers</span></summary>
        <div class="sect-body">
          {#each mcpServers as server}
            <div class="list-row"><div class="name">{server}</div></div>
          {:else}
            <div class="muted">No MCP servers are configured.</div>
          {/each}
          {#if provider === 'claude' && configSources.length}
            <div class="context-subhead">Configuration sources present</div>
            {#each configSources as source (source.path)}
              <div class="list-row">
                <div class="name">{source.path}</div>
                <div class="desc">{source.kind} · {source.scope}</div>
              </div>
            {/each}
          {/if}
          <div class="context-note">Configuration only. Historical inspection does not connect to MCP servers.</div>
        </div>
      </details>

      <details class="sect" open>
        <summary>Permissions &amp; Runtime</summary>
        <div class="sect-body">
          {#if provider === 'claude'}
            <dl class="kv">
              <dt>last model used</dt><dd>{display(values.model)} <span class="n">recorded in transcript</span></dd>
              <dt>configured model now</dt><dd>{display(values.configuredModel, 'Claude default')}</dd>
              <dt>last effort</dt><dd>{display(values.reasoningEffort)} <span class="n">recorded in transcript</span></dd>
              <dt>configured effort now</dt><dd>{display(values.configuredEffort, 'Claude default')}</dd>
              <dt>last permission mode</dt><dd>{display(values.permissionMode)} <span class="n">recorded in transcript</span></dd>
              <dt>configured permission now</dt><dd>{display(values.configuredPermissionMode, 'Claude default')}</dd>
              <dt>permission rules</dt><dd>{permissions.allow?.length ?? 0} allow · {permissions.ask?.length ?? 0} ask · {permissions.deny?.length ?? 0} deny</dd>
              <dt>output style</dt><dd>{display(values.outputStyle, 'default')}</dd>
              <dt>Claude version</dt><dd>{display(values.recordedClaudeVersion)} <span class="n">then · {display(values.currentClaudeVersion)} installed now</span></dd>
              <dt>cwd</dt><dd>{display(values.cwd)}</dd>
            </dl>
          {:else}
            <dl class="kv">
              <dt>model</dt><dd>{display(values.model)} <span class="n">({display(values.reasoningEffort, 'default')} effort{values.serviceTier?.state === 'known' ? `, ${display(values.serviceTier)}` : ''})</span></dd>
              <dt>provider</dt><dd>{display(values.modelProvider)}</dd>
              <dt>sandbox</dt><dd>{display(values.sandbox)}</dd>
              <dt>approvals</dt><dd>{display(values.approvalPolicy)} <span class="n">reviewer: {display(values.approvalsReviewer)}</span></dd>
              <dt>permission profile</dt><dd>{display(values.permissionProfile)}</dd>
              <dt>cwd</dt><dd>{display(values.cwd)}</dd>
              <dt>workspace roots</dt><dd>{workspaceRootsValue()}</dd>
              <dt>multi-agent</dt><dd>{display(values.multiAgentMode)}</dd>
            </dl>
          {/if}
        </div>
      </details>

      <details class="sect">
        <summary>Available Skills{provider === 'claude' ? ' & Commands' : ''} <span class="n">{skills.length}</span></summary>
        <div class="sect-body">
          {#each skills as skill (skill.path || skill.name)}
            <div class="list-row">
              <div class="name">{skill.name}</div>
              {#if skill.description}<div class="desc">{skill.description}</div>{/if}
            </div>
          {:else}
            <div class="muted">No skills are currently available.</div>
          {/each}
          {#if provider === 'claude' && plugins.length}
            <div class="context-subhead">Enabled plugins</div>
            <div class="list-row"><div class="desc">{plugins.join(', ')}</div></div>
          {/if}
        </div>
      </details>

      {#if provider === 'claude'}
        <details class="sect">
          <summary>Evidence Boundary <span class="n">documented inference</span></summary>
          <div class="sect-body">
            <dl class="kv">
              <dt>assembled prompt</dt><dd>not exposed by Claude</dd>
              <dt>tool inventory</dt><dd>live only</dd>
              <dt>MCP health</dt><dd>live only</dd>
              <dt>inspection behavior</dt><dd>filesystem reads only</dd>
            </dl>
            <div class="context-note">Opening this thread did not resume Claude, run hooks, start MCP servers, or modify Claude-owned files.</div>
          </div>
        </details>
      {/if}

      <details class="sect">
        <summary>Workspace State <span class="n">observed now</span></summary>
        <div class="sect-body">
          {#if git}
            <dl class="kv">
              <dt>git root</dt><dd>{git.root || '—'}</dd>
              <dt>branch</dt><dd>{git.branch || 'detached'}</dd>
              <dt>commit</dt><dd>{git.commit || '—'}</dd>
              <dt>tracked changes</dt><dd>{git.dirty == null ? 'unknown' : git.dirty ? 'yes' : 'no'}</dd>
            </dl>
          {:else}
            <div class="muted">This working directory is not in a Git repository.</div>
          {/if}
        </div>
      </details>

      {#if recordedDifferences.length}
        <details class="sect">
          <summary>Changed Since This Thread Ran <span class="n">{recordedDifferences.length}</span></summary>
          <div class="sect-body">
            <dl class="kv">
              {#each recordedDifferences as difference}
                <dt>{difference.label}</dt><dd>{difference.recorded} <span class="n">then · {difference.current} now</span></dd>
              {/each}
            </dl>
          </div>
        </details>
      {/if}

      {#if resolution.diagnostics?.length}
        <details class="sect">
          <summary>Diagnostics <span class="n">{resolution.diagnostics.length}</span></summary>
          <div class="sect-body">
            {#each resolution.diagnostics as diagnostic}<div class="context-note">{diagnostic}</div>{/each}
          </div>
        </details>
      {/if}
    </div>
  {/if}
</section>
