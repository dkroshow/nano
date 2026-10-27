import { mount } from 'svelte'
import './styles/nano.css'
import { applySavedTextSize } from './lib/text_size.js'
import { applySavedTheme } from './lib/theme.js'
import { installNetworkMeter } from './lib/network_meter.js'
import App from './App.svelte'

// Before the app opens any request or stream; a no-op unless the meter is on.
installNetworkMeter()

// Keep runtime state in sync with the synchronous boot script in index.html.
applySavedTheme()
applySavedTextSize()

export default mount(App, { target: document.getElementById('app') })
