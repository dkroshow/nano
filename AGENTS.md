# nano agent guide

Nano is a Go server with an embedded Svelte web app. `internal/app` is the
server; `internal/` also holds the packages it is built from. `cmd/nanod` is
the entry point, `cmd/` holds the helper binaries, and `web/` is the UI. Read
`docs/architecture.md` before a change that crosses a package boundary, and
`docs/principles.md` before any design choice: where documents disagree,
`docs/principles.md` wins.

## Setup, build, test

```sh
npm --prefix web ci                  # web dependencies (lockfile)
npm --prefix web run build           # web/dist, which the server embeds
go build -o nanod ./cmd/nanod && ./nanod       # http://127.0.0.1:7690
go test -timeout 30m ./...           # allow for the slow app test binary
npm --prefix web test                # web logic and component tests
npm --prefix web run dev             # UI loop on http://localhost:5173
```

The one check before a commit:

```sh
gofmt -l . && go vet ./... && go test -timeout 30m ./... && npm --prefix web test
```

`gofmt -l .` must print nothing. CI runs the same steps on macOS.

## Conventions an agent could not guess

- **The web bundle is embedded** (`//go:embed all:dist` in `web/embed.go`).
  A UI change shows nothing until `npm --prefix web run build` and a fresh
  `go build`. During UI work use the dev server instead: it proxies `/api`
  and `/events` to a `nanod` already running on :7690, which must stay up.
  `localStorage` is per port, so :5173 starts with default view settings.
- **Component tests render real Svelte components on jsdom.** A file opts in
  with `// @vitest-environment jsdom` as its first line; everything else runs
  on Node. `web/src/test_setup.js` shims `localStorage`. Stub `EventSource`
  and `fetch` before rendering anything that mounts a session store or
  durable history (see `web/src/lib/panes/thread_pane_view.test.js`). The
  `resolve.conditions: ['browser']` setting in `web/vite.config.js` is
  guarded by `process.env.VITEST`; do not unguard it.
- **Test at the owner, once.** The live-text loop owns the saved/live
  handoff, the store owns durable state, the HTTP API owns what agents see,
  and a rendered component owns what the operator sees. Put a behavior's one
  primary test at the boundary that owns it; a second test elsewhere needs a
  failure the first cannot catch.
- **Provider files are read-only.** Nano never writes under `~/.codex`,
  `~/.claude` or `~/.grok`; provider processes create their own sessions.
  Keep it that way.
- **Transcripts come from the saved file.** Panes and agent reads show rows
  from the provider's own transcript file, plus unsaved live text. Nano keeps
  no copy of transcript rows in memory; do not add one.
- **Fixtures are synthetic or scrubbed.** `internal/app/testdata/` holds small captured
  shapes with invented ids, paths and text. Never add a real session.
- **No always-on work without a measured reason.** No new polling, per-frame
  encoding or whole-store scans; carry the fact in the event that already
  exists. Log on change, not on every pass.
- Flags are the configuration surface; `internal/config/config.go` parses all of them into
  one value before any store or listener opens. Four environment variables
  are read beside them: `CODEX_NANO_CLAUDE_BIN` and `CODEX_NANO_GROK_BIN`
  name the CLI executables when the flags are empty, and the two switches
  `NANO_LIVE_TEXT_DELTAS=off` and `NANO_SUPERSEDED_COLLECTION=on` exist so
  a rollback to an older binary keeps the same launch command. Do not add
  more.
- `scripts/build-helper.sh claude-shim|nano-launcher` builds a helper binary
  into the repo root under its ignored name (`claude-shim-bin`,
  `nano-launcher-bin`), unsigned by default; with `NANO_CODESIGN_IDENTITY`
  set it codesigns the result so a macOS privacy grant survives rebuilds.

## Never

- Never commit secrets, tokens, real session files or personal paths.
- Never weaken, skip or delete a test to get green. A failing test is either
  a real bug to fix or a useless test to remove, with the reason stated.
- Never claim tests passed when they did not run or did not pass. Report the
  exact command and result.
- Never widen scope. Do what was asked; name anything else you noticed.
- Never add AI attribution to commits: no `Co-authored-by`, `Generated with`
  or similar trailers, and no bot identities.
- Never write to `~/.codex`, `~/.claude` or `~/.grok`.
