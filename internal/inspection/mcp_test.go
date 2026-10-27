package inspection

import (
	"encoding/json"
	"testing"

	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

func TestMCPProjectionSortsToolsAndDropsSchemas(t *testing.T) {
	inventory := ProjectMCPInventory(json.RawMessage(`{"data":[{"name":"docs","tools":{"z":{"inputSchema":"secret"},"a":{}}}]}`))
	if len(inventory.Servers) != 1 || len(inventory.Servers[0].Tools) != 2 || inventory.Servers[0].Tools[0] != "a" {
		t.Fatalf("inventory = %#v", inventory)
	}
	if !MCPInventoryStale("codex", protocol.Recv, []byte(`{"method":"mcpServer/startupStatus/updated"}`)) {
		t.Fatal("startup transition did not invalidate the inventory")
	}
}

func TestMCPCacheRejectsInvalidatedFetch(t *testing.T) {
	cache := &MCPCache{}
	_, generation := cache.Cached()
	cache.Invalidate()
	cache.Store(&MCPInventory{Servers: []MCPServer{{Name: "stale"}}}, generation)
	if inventory, _ := cache.Cached(); inventory != nil {
		t.Fatalf("stale inventory was cached: %#v", inventory)
	}
}
