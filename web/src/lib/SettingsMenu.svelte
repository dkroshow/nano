<script>
  // Fixed top-right gear → dropdown → Thread Start Parameters modal.
  import { onMount } from 'svelte'
  import ThreadStartParamsModal from './ThreadStartParamsModal.svelte'
  import { SHORTCUT_LEGEND } from './keyboard.js'
  import { TEXT_SIZES, applyTextSize, savedTextSizeIdx } from './text_size.js'
  import { applyTheme, savedTheme } from './theme.js'
  import { networkMeterEnabled, setNetworkMeter } from './network_meter.js'

  let menuOpen = $state(false)
  let modalOpen = $state(false)
  let textSizeIdx = $state(savedTextSizeIdx())
  let theme = $state(savedTheme())
  let remoteProtected = $state(false)

  onMount(() => {
    let active = true
    void fetch('/auth/status', { headers: { accept: 'application/json' } })
      .then((response) => response.ok ? response.json() : null)
      .then((status) => {
        if (active) remoteProtected = status?.remote === true && status?.authenticated === true
      })
      .catch(() => {})
    return () => { active = false }
  })

  function pickTextSize(i) {
    textSizeIdx = applyTextSize(i)
  }
  function toggleTheme() {
    theme = applyTheme(theme === 'light' ? 'dark' : 'light')
  }
  let root // gear + dropdown wrapper, for outside-click detection

  function toggle() {
    menuOpen = !menuOpen
  }
  function openParams() {
    menuOpen = false
    modalOpen = true
  }
  function onWindowClick(e) {
    if (menuOpen && root && !root.contains(e.target)) menuOpen = false
  }
  function onKeydown(e) {
    if (e.key === 'Escape' && menuOpen) menuOpen = false
  }
</script>

<svelte:window onclick={onWindowClick} onkeydown={onKeydown} />

<div class="sg-root" bind:this={root}>
  <button class="sg-btn" onclick={toggle} title="Settings" aria-label="Settings">⚙</button>
  {#if menuOpen}
    <div class="sg-menu">
      <button class="sg-item" onclick={openParams}>Thread Start Parameters</button>
      <a class="sg-item" href="/security">Remote Security</a>
      <div class="sg-section">
        <div class="sg-theme-row">
          <span>Theme</span>
          <button
            class="sg-theme-toggle"
            class:on={theme === 'light'}
            aria-pressed={theme === 'light'}
            onclick={toggleTheme}
          >{theme === 'light' ? 'Light' : 'Dark'}</button>
        </div>
      </div>
      <div class="sg-section">
        <div class="sg-theme-row">
          <span title="Debug: bytes this tab sends and receives. Turning it on or off reloads the page.">Network meter</span>
          <button
            class="sg-theme-toggle"
            class:on={networkMeterEnabled()}
            aria-pressed={networkMeterEnabled()}
            onclick={() => setNetworkMeter(!networkMeterEnabled())}
          >{networkMeterEnabled() ? 'On' : 'Off'}</button>
        </div>
      </div>
      <div class="sg-section">
        <div class="sg-label">Text size</div>
        <div class="sg-size-btns" role="radiogroup" aria-label="Text size">
          {#each TEXT_SIZES as size, i}
            <button
              class="sg-size-btn"
              class:on={textSizeIdx === i}
              aria-pressed={textSizeIdx === i}
              onclick={() => pickTextSize(i)}
            >{size.label}</button>
          {/each}
        </div>
      </div>
      <div class="sg-section">
        <div class="sg-label">Shortcuts</div>
        {#each SHORTCUT_LEGEND as shortcut}
          <div class="sg-shortcut">
            <span class="sg-key">{shortcut.keys}</span>
            <span>{shortcut.label}</span>
          </div>
        {/each}
      </div>
      {#if remoteProtected}
        <form class="sg-lock-form" method="post" action="/auth/logout">
          <button class="sg-item sg-lock" type="submit">Lock Nano</button>
        </form>
      {/if}
    </div>
  {/if}
</div>

{#if modalOpen}
  <ThreadStartParamsModal onClose={() => (modalOpen = false)} />
{/if}
