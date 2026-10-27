package codex_test

import (
	"context"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/wire/codex"
	"github.com/dkroshow/nano/internal/wire/wiretest"
)

func TestClientUsesSharedFakeServer(t *testing.T) {
	server := wiretest.NewServer(t)
	client := codex.NewClientWS(server.URL())
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.Call(ctx, "initialize", map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}
}
