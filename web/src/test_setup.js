// Shared vitest setup.
//
// Node defines a global `localStorage` that stays undefined unless the runtime
// is started with --localstorage-file, and it shadows the one jsdom provides.
// Components read it at init (text size, transcript view, density), so without
// this a component test dies on `localStorage.getItem` before it renders.
// In-memory and per-process: tests that care about persistence set values
// themselves rather than inheriting a previous run's.
if (!globalThis.localStorage?.getItem) {
  const store = new Map()
  Object.defineProperty(globalThis, 'localStorage', {
    configurable: true,
    writable: true,
    value: {
      getItem: (key) => (store.has(String(key)) ? store.get(String(key)) : null),
      setItem: (key, value) => store.set(String(key), String(value)),
      removeItem: (key) => store.delete(String(key)),
      clear: () => store.clear(),
      key: (index) => [...store.keys()][index] ?? null,
      get length() {
        return store.size
      },
    },
  })
}
