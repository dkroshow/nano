package inspection

import (
	"encoding/json"
	"sort"
	"sync"

	"github.com/dkroshow/nano/internal/wire"
	protocol "github.com/dkroshow/nano/internal/wire/codex"
)

type MCPInventory struct {
	Servers []MCPServer `json:"servers"`
}

type MCPServer struct {
	Name  string   `json:"name"`
	Tools []string `json:"tools"`
}

type MCPCache struct {
	mu         sync.Mutex
	inventory  *MCPInventory
	generation uint64
}

func (cache *MCPCache) Invalidate() {
	cache.mu.Lock()
	cache.inventory = nil
	cache.generation++
	cache.mu.Unlock()
}

func (cache *MCPCache) Cached() (*MCPInventory, uint64) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return cache.inventory, cache.generation
}

func (cache *MCPCache) Store(inventory *MCPInventory, generation uint64) {
	cache.mu.Lock()
	if cache.generation == generation {
		cache.inventory = inventory
	}
	cache.mu.Unlock()
}

func MCPInventoryStale(provider string, direction wire.Direction, line []byte) bool {
	if provider != "codex" || direction != protocol.Recv {
		return false
	}
	var message struct {
		Method string `json:"method"`
	}
	return json.Unmarshal(line, &message) == nil && message.Method == "mcpServer/startupStatus/updated"
}

func ProjectMCPInventory(raw json.RawMessage) *MCPInventory {
	var response struct {
		Data []struct {
			Name  string                     `json:"name"`
			Tools map[string]json.RawMessage `json:"tools"`
		} `json:"data"`
	}
	inventory := &MCPInventory{Servers: []MCPServer{}}
	if json.Unmarshal(raw, &response) != nil {
		return inventory
	}
	for _, server := range response.Data {
		tools := make([]string, 0, len(server.Tools))
		for name := range server.Tools {
			tools = append(tools, name)
		}
		sort.Strings(tools)
		inventory.Servers = append(inventory.Servers, MCPServer{Name: server.Name, Tools: tools})
	}
	return inventory
}
