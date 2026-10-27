// Command claude-shim is a small daemon that owns `claude -p` children so they
// outlive restarts of the nano server — the Claude analog of the supervised
// codex app-server daemon.
//
// Nano asks the shim to start a child, then attaches over WebSocket and pumps
// stdio lines through it. Detaching is implicit — the WebSocket drops when
// nano restarts — and the child keeps running with every line it emits
// sequenced into a spool, so the next nano attaches with a sequence number and
// replays exactly the gap. Nothing is translated: lines cross verbatim in both
// directions.
//
// Explicitly transitional: the exit conditions are Anthropic shipping a native
// attach surface, or the Thread Service absorbing session hosting.
//
//	claude-shim [-listen 127.0.0.1:7692] [-spool-dir ~/.nanod/claude-shim/spools]
package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/dkroshow/nano/internal/configpaths"
)

const defaultListen = "127.0.0.1:7692"

func main() {
	listen := flag.String("listen", defaultListen, "address to serve the WebSocket JSON-RPC surface on")
	spoolDir := flag.String("spool-dir", "", "directory for per-session spools (default ~/.nanod/claude-shim/spools)")
	flag.Parse()

	dir, err := resolveSpoolDir(*spoolDir)
	if err != nil {
		log.Fatalf("resolving spool dir: %v", err)
	}
	// Children die with the shim. Nano's `--resume` fallback reuses the
	// retained spool sequence so provider turn ids never restart at one.
	if err := prepareSpoolDir(dir); err != nil {
		log.Fatalf("preparing spool dir %s: %v", dir, err)
	}

	mux := http.NewServeMux()
	mux.Handle("/", newServer(dir))
	log.Printf("claude-shim listening on %s (spools: %s)", *listen, dir)
	if err := http.ListenAndServe(*listen, mux); err != nil {
		log.Fatalf("serving on %s: %v", *listen, err)
	}
}

func resolveSpoolDir(configured string) (string, error) {
	if configured != "" {
		return configured, nil
	}
	return configpaths.DefaultPath("claude-shim", "spools")
}
