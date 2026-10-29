// mcp_inventory.go — the MCP tool registry for the Responses Input panel,
// served off the /state critical path.
//
// Why it is its own endpoint: `mcpServerStatus/list` measures 0.84–1.5s and
// ~235 KB against a real app-server (6.1s once, when a freshly resumed
// thread's servers were still starting), and the panel renders only server
// names and tool names from it. Inside /state that single call gated the whole
// right sidebar — and, through the frontend's connect ordering, the wire stream
// behind it. So: fetch it lazily when the Tool Registry section is expanded,
// project it down to what the panel renders, and cache it until the wire says a
// server's startup state changed.
package sessions

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/inspection"
)

const mcpInventoryTimeout = 30 * time.Second

// mcpInventoryDetail asks app-server to skip resource/resourceTemplate
// inventories. Tool names — all the panel needs — survive intact
// (verified against the real daemon: 0.84s/215 KB vs 1.4s/235 KB for `full`).
const mcpInventoryDetail = "toolsAndAuthOnly"

// mcpServers returns the cached registry, fetching it from app-server on a
// miss. The app-server call deliberately runs without the cache lock held: it
// can take seconds, and an invalidation arriving from the wire in the meantime
// must not block behind it.
func (sess *View) mcpServers(ctx context.Context) (*inspection.MCPInventory, error) {
	cached, generation := sess.mcp.Cached()
	if cached != nil {
		return cached, nil
	}

	var raw json.RawMessage
	params := map[string]any{"threadId": sess.session.stateOwner.ThreadID(), "detail": mcpInventoryDetail}
	if err := sess.session.runtime.codex.callForSession(ctx, sess.session, "mcpServerStatus/list", params, &raw); err != nil {
		return nil, err
	}
	inventory := inspection.ProjectMCPInventory(raw)
	sess.mcp.Store(inventory, generation)
	return inventory, nil
}

// HandleMCPServers serves the attachment provider MCP inventory.
func (sess *View) HandleMCPServers(w http.ResponseWriter, r *http.Request) {
	if sess.session.provider != "codex" {
		// Claude's registry comes from its system.init snapshot, already on /state.
		httpapi.WriteHTTPError(w, "mcp inventory is codex-only", http.StatusNotImplemented)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), mcpInventoryTimeout)
	defer cancel()
	inventory, err := sess.mcpServers(ctx)
	if err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadGateway)
		return
	}
	httpapi.WriteJSON(w, inventory)
}
