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
package main

import (
	"context"
	"encoding/json"
	"github.com/dkroshow/nano/internal/httpapi"
	"net/http"
	"time"

	"github.com/dkroshow/nano/internal/inspection"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

const mcpInventoryTimeout = 30 * time.Second

// mcpInventoryDetail asks app-server to skip resource/resourceTemplate
// inventories. Tool names — all the panel needs — survive intact
// (verified against the real daemon: 0.84s/215 KB vs 1.4s/235 KB for `full`).
const mcpInventoryDetail = "toolsAndAuthOnly"

// mcpInventory is the projected registry: server names plus their tool names.
// Deliberately not the raw response — codex_apps alone ships 244 KB of tool
// schemas that nothing reads.
type mcpInventory = inspection.MCPInventory
type mcpServer = inspection.MCPServer

// mcpInventoryCache is a session's cached registry. The generation counter is
// what makes lock-free fetching safe: an invalidation that lands while a fetch
// is in flight bumps it, so the completing fetch serves its result to the
// caller but declines to cache a snapshot it already knows is stale.
type mcpInventoryCache struct{ inspection.MCPCache }

func (c *mcpInventoryCache) invalidate() {
	c.Invalidate()
}

func (c *mcpInventoryCache) cached() (*mcpInventory, uint64) {
	return c.Cached()
}

func (c *mcpInventoryCache) store(inventory *mcpInventory, generation uint64) {
	c.Store(inventory, generation)
}

// mcpInventoryStale reports whether a wire line means the cached registry can
// no longer be trusted: tool inventories fill in as MCP servers start, which is
// exactly what mcpServer/startupStatus/updated announces. Any status counts —
// a server going away invalidates just as much as one becoming ready.
func mcpInventoryStale(provider string, dir protocol.Direction, line []byte) bool {
	return inspection.MCPInventoryStale(provider, dir, line)
}

// mcpServers returns the cached registry, fetching it from app-server on a
// miss. The app-server call deliberately runs without the cache lock held: it
// can take seconds, and an invalidation arriving from the wire in the meantime
// must not block behind it.
func (sess *session) mcpServers(ctx context.Context) (*mcpInventory, error) {
	cached, generation := sess.mcp.cached()
	if cached != nil {
		return cached, nil
	}

	var raw json.RawMessage
	params := map[string]any{"threadId": sess.currentThreadID(), "detail": mcpInventoryDetail}
	if err := sess.codex.CallForSession(ctx, sess, "mcpServerStatus/list", params, &raw); err != nil {
		return nil, err
	}
	inventory := projectMCPInventory(raw)
	sess.mcp.store(inventory, generation)
	return inventory, nil
}

// projectMCPInventory reduces a ListMcpServerStatusResponse to names and tool
// names. McpServerStatus.tools is a map keyed by tool name, so the keys are the
// registry; sorting them keeps the panel's rendering stable across fetches.
func projectMCPInventory(raw json.RawMessage) *mcpInventory {
	return inspection.ProjectMCPInventory(raw)
}

func (sess *session) handleMCPServers(w http.ResponseWriter, r *http.Request) {
	if sess.provider != "codex" {
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
