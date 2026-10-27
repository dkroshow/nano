package claude_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/wire/claude"
	"github.com/dkroshow/nano/internal/wire/wiretest"
)

func TestClientUsesSharedFakeServer(t *testing.T) {
	server := wiretest.NewServer(t)
	client, err := claude.Dial(server.URL(), time.Second, func(string, json.RawMessage) {}, func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.Call(ctx, "session/start", map[string]any{}, nil, time.Second); err != nil {
		t.Fatal(err)
	}
}
