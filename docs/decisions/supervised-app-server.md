# Supervised Codex app server

A separately supervised Codex daemon keeps hosted sessions running across
Nano restarts. Nano dials it first. When its launchd job is loaded but the
socket is not ready, Nano waits instead of starting a competing daemon.
When no job is loaded, a delayed fallback can start Codex and logs a warning.

`-codex-supervisor-label` selects the job to inspect. Its default is
`local.nano.app-server`; installations with another label must pass it
explicitly. The endpoint selected by `-codex-ws` must match the daemon's
`--listen` endpoint.

[The app-server template](../../tools/local.nano.app-server.plist) uses
`RunAtLoad`, `KeepAlive`, and a retry throttle. Replace its placeholder paths
before installation. Restart the provider daemon deliberately after a Codex
upgrade, with no active turns; restarting Nano alone does not upgrade it.
