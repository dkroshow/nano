import { defineConfig } from 'vite'
import { svelte } from '@sveltejs/vite-plugin-svelte'

export default defineConfig({
  plugins: [svelte()],
  server: {
    proxy: {
      '/events': 'http://localhost:7690',
      '/api': 'http://localhost:7690',
    },
  },
  // Svelte 5 ships separate server and client builds. Component tests must get
  // the client one (the server build renders to a string and has no DOM), which
  // resolves through the browser condition. Scoped to test runs so the app
  // build is untouched.
  resolve: process.env.VITEST ? { conditions: ['browser'] } : undefined,
  test: {
    // Logic modules stay on the fast node environment; a component test opts
    // into a DOM with `// @vitest-environment jsdom` at the top of its file.
    environment: 'node',
    setupFiles: ['./src/test_setup.js'],
  },
})
