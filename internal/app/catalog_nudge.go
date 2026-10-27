// catalog_nudge.go — wire hints for nano-hosted sessions
// (docs/decisions/freshness-follows-hosting.md). A turn edge names the
// session's known native file, which the catalog queues like a file-watch
// event. The hint is allowed to be wrong: a missed one is covered by the
// file's own watch, and a spurious one is a cheap no-op pass. It never asks
// for a provider-root walk.
package app

import "encoding/json"

// Identity-binding wire lines — where a hosted session's native thread id first
// becomes knowable, before any turn edge has happened. codex announces the
// thread object (id, rollout path) on thread/started; claude's analog is the
// system.init handshake carrying session_id (claude.go captureInit).
const (
	codexThreadStartedMethod = "thread/started"
	claudeSystemLineType     = "system"
	claudeInitSubtype        = "init"
)

func codexCatalogPath(line []byte) string {
	var envelope struct {
		Method string `json:"method"`
		Params struct {
			Thread struct {
				Path string `json:"path"`
			} `json:"thread"`
		} `json:"params"`
	}
	if json.Unmarshal(line, &envelope) != nil || envelope.Method != codexThreadStartedMethod {
		return ""
	}
	return envelope.Params.Thread.Path
}

// wireNudgesCatalog reports whether one recorded wire line is a TURN EDGE (or
// the identity binding that precedes the first one) worth a catalog refresh.
//
// Only edges qualify, not every durable-progress line. Mid-turn there is no
// consumer of durable catalog freshness: live panes are wire-fed, and the
// sidebar's working state comes from the volatile runtime overlay (runtime.go),
// neither of which reads the scanned catalog. Nudging on every item/completed
// and thread/tokenUsage/updated therefore bought nothing and cost everything —
// with a handful of hosted sessions it arrived every second or two and held the
// scanner in permanent back-to-back full-scan cycles.
//
// The START edge nudges too, not just completion: the fleet feed
// (/api/v1/fleet/changes) derives its `working` transition from the sidebar
// rows a catalog version bump repairs (fleet_changes.go), so a turn that only
// announced itself at the end
// would never be seen as in flight.
//
// Direction matters per provider. Codex opens a turn with a received
// turn/started notification, so nano's own requests and codex's responses never
// qualify. Claude has no turn-start line of its own — nano's SENT user line IS
// the edge (turnActiveTransition reads it the same way), which is the single
// case where sent traffic nudges. A RECEIVED claude user line is a tool result
// mid-turn and does not.
