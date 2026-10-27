# One Codex app server

All Codex threads share an app-server daemon. Nano multiplexes thread-scoped
requests and notifications over one WebSocket connection. `-codex-ws` selects
the endpoint, defaulting to `ws://127.0.0.1:7691`.

A daemon separates the provider lifetime from the UI server lifetime and
avoids starting a copy of every configured MCP server for each thread.
Closing a thread unsubscribes its attachment without stopping the daemon.
Connection-scoped requests remain distinct from thread-scoped traffic.
See [supervision](supervised-app-server.md) for the startup guard.
