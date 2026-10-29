<script>
  // The network meter's panel, mounted only while the meter is on.
  import { onMount } from 'svelte'
  import { SENT_HEADER_ESTIMATE, networkMeterSnapshot, setNetworkMeter } from './network_meter.js'

  let open = $state(false)
  let snapshot = $state(networkMeterSnapshot())

  // The counters are plain numbers, so the panel reads them each second.
  onMount(() => {
    const timer = setInterval(() => (snapshot = networkMeterSnapshot()), 1000)
    return () => clearInterval(timer)
  })

  function size(bytes) {
    if (bytes < 1000) return `${bytes} B`
    if (bytes < 1e6) return `${(bytes / 1e3).toFixed(1)} KB`
    return `${(bytes / 1e6).toFixed(2)} MB`
  }
</script>

<div class="nm-root">
  <button class="nm-badge" onclick={() => (open = !open)} aria-expanded={open} title="Network meter: this tab since load">
    ↓ {size(snapshot.received)} · ↑ {size(snapshot.sent)}
  </button>
  {#if open}
    <div class="nm-panel" role="dialog" aria-label="Network meter">
      <div class="nm-head">
        <strong>Network meter</strong>
        <span>this tab, since load</span>
        <button class="nm-off" onclick={() => setNetworkMeter(false)}>Turn off</button>
      </div>
      <div class="nm-minutes" aria-label="Per minute, newest first">
        {#each snapshot.minutes as minute (minute.minute)}
          <span title="Minute {minute.minute + 1} after load">m{minute.minute + 1} ↓{size(minute.received)} ↑{size(minute.sent)}</span>
        {/each}
      </div>
      <table class="nm-table">
        <thead>
          <tr><th>Endpoint</th><th>Reqs</th><th>↓ total</th><th>↑ total</th><th>↓ last min</th><th>↑ last min</th></tr>
        </thead>
        <tbody>
          {#each snapshot.routes as route (route.key)}
            <tr>
              <td class="nm-key">{route.stream ? 'stream ' : ''}{route.key}</td>
              <td>{route.requests}</td>
              <td>{size(route.received)}</td>
              <td>{size(route.sent)}</td>
              <td>{size(route.lastMinuteReceived)}</td>
              <td>{size(route.lastMinuteSent)}</td>
            </tr>
          {/each}
        </tbody>
      </table>
      <p class="nm-note">
        ↓ is bytes on the wire, from the browser's request timings. A stream counts the event data it delivers,
        before compression, so a compressed stream costs less on the wire; event types this page does not listen for
        are not counted.
        ↑ is request bodies plus an estimated {SENT_HEADER_ESTIMATE} B of headers per request.
        "Last min" is the last full minute; a request counts in the minute it completes.
      </p>
    </div>
  {/if}
</div>
