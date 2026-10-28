package sessions

import (
	"bytes"
	"testing"
	"time"
)

// Catches: a Codex daemon upgraded under a running Nano (the provider gates new
// models such as gpt-6-sol by client version) while Nano keeps serving the old
// daemon's cached model/list until its own restart; and, while the transport
// is down, a stalled request or error instead of the last catalog. Boundary:
// the daemon transport dropping and reconnecting, observed through fetchModels.
func TestCodexModelCatalogRefetchesAfterDaemonReconnect(t *testing.T) {
	fake := newFakeAppServer(t)
	fake.Mu.Lock()
	fake.ModelList = map[string]any{"data": []any{map[string]any{"model": "gpt-5.6-sol"}}}
	fake.Mu.Unlock()
	d := fake.daemon()
	t.Cleanup(func() {
		d.mu.Lock()
		client := d.client
		d.mu.Unlock()
		if client != nil {
			client.Close()
		}
	})

	first, err := d.FetchModels(false)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(first, []byte("gpt-5.6-sol")) {
		t.Fatalf("first catalog = %s", first)
	}

	fake.Mu.Lock()
	fake.ModelList = map[string]any{"data": []any{map[string]any{"model": "gpt-6-sol"}}}
	fake.Mu.Unlock()
	d.mu.Lock()
	oldClient := d.client
	d.mu.Unlock()
	fake.DropConnections()
	<-oldClient.Done()

	// Between transports the last catalog is served at once, not a stalled
	// request or an error.
	if got, err := d.FetchModels(false); err != nil || !bytes.Contains(got, []byte("gpt-5.6-sol")) {
		t.Fatalf("catalog while disconnected = %s, err %v", got, err)
	}

	// Nano's session recovery reconnects the transport; the next fetch after
	// that must come from the new daemon.
	if err := d.ensure(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := d.FetchModels(false)
		if err == nil && bytes.Contains(got, []byte("gpt-6-sol")) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("catalog after reconnect = %s, err %v", got, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
