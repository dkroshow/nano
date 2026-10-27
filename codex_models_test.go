package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestEnsureCodexAstraModelAppendsFallback(t *testing.T) {
	input := json.RawMessage(`{"data":[{"model":"gpt-5.6-sol","displayName":"Sol"}],"nextCursor":"cursor"}`)
	merged, err := ensureCodexAstraModel(input)
	if err != nil {
		t.Fatal(err)
	}

	var got struct {
		Data []struct {
			Model                     string `json:"model"`
			DisplayName               string `json:"displayName"`
			DefaultReasoningEffort    string `json:"defaultReasoningEffort"`
			SupportedReasoningEfforts []struct {
				ReasoningEffort string `json:"reasoningEffort"`
			} `json:"supportedReasoningEfforts"`
		} `json:"data"`
		NextCursor string `json:"nextCursor"`
	}
	if err := json.Unmarshal(merged, &got); err != nil {
		t.Fatal(err)
	}
	if got.NextCursor != "cursor" || len(got.Data) != 2 {
		t.Fatalf("merged catalog = %s", merged)
	}
	astra := got.Data[1]
	if astra.Model != "gpt-6-astra" || astra.DisplayName != "GPT-6-Astra" || astra.DefaultReasoningEffort != "low" {
		t.Fatalf("Astra fallback = %+v", astra)
	}
	wantEfforts := []string{"low", "medium", "high", "xhigh", "max", "ultra"}
	if len(astra.SupportedReasoningEfforts) != len(wantEfforts) {
		t.Fatalf("Astra efforts = %+v", astra.SupportedReasoningEfforts)
	}
	for i, want := range wantEfforts {
		if got := astra.SupportedReasoningEfforts[i].ReasoningEffort; got != want {
			t.Fatalf("Astra effort %d = %q, want %q", i, got, want)
		}
	}
}

func TestEnsureCodexAstraModelPreservesProviderEntry(t *testing.T) {
	input := json.RawMessage(`{"data":[{"model":"gpt-6-astra","displayName":"Provider Astra","futureField":true}]}`)
	merged, err := ensureCodexAstraModel(input)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(merged, input) {
		t.Fatalf("provider catalog changed:\n got %s\nwant %s", merged, input)
	}
}

// The New Window UI selects models from model/list, then shows this exact
// preview before spawning. Keep GPT-6.1 Sol, GPT-6 Astra, Sol, Luna and the current GPT-5.6 family covered here so a
// future change cannot silently drop or rewrite one of the selected slugs.
func TestPinnedCodexModelsPreviewAsSelected(t *testing.T) {
	s := &server{defaults: sessionDefaults{
		cwd:              t.TempDir(),
		sandbox:          "read-only",
		approval:         "never",
		reasoningSummary: "detailed",
	}}

	for _, model := range []string{"gpt-6.1-sol", "gpt-6-astra", "gpt-6-sol", "gpt-6-luna", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"} {
		t.Run(model, func(t *testing.T) {
			body, err := json.Marshal(map[string]string{
				"provider": "codex",
				"model":    model,
				"effort":   "max",
			})
			if err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/sessions/preview", bytes.NewReader(body))
			s.handleSpawnPreview(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}

			var got struct {
				Method        string         `json:"method"`
				Params        map[string]any `json:"params"`
				TurnOverrides map[string]any `json:"turnOverrides"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if got.Method != "thread/start" {
				t.Fatalf("method = %q, want thread/start", got.Method)
			}
			if got.Params["model"] != model {
				t.Fatalf("params.model = %#v, want %q", got.Params["model"], model)
			}
			if got.TurnOverrides["effort"] != "max" {
				t.Fatalf("turnOverrides.effort = %#v, want max", got.TurnOverrides["effort"])
			}
		})
	}
}

// Catches: a Codex daemon upgraded under a running Nano (the provider gates new
// models such as gpt-6-sol by client version) while Nano keeps serving the old
// daemon's cached model/list until its own restart; and, while the transport
// is down, a stalled request or error instead of the last catalog. Boundary:
// the daemon transport dropping and reconnecting, observed through fetchModels.
func TestCodexModelCatalogRefetchesAfterDaemonReconnect(t *testing.T) {
	fake := newFakeAppServer(t)
	fake.mu.Lock()
	fake.modelList = map[string]any{"data": []any{map[string]any{"model": "gpt-5.6-sol"}}}
	fake.mu.Unlock()
	d := fake.daemon()
	t.Cleanup(func() {
		d.mu.Lock()
		client := d.client
		d.mu.Unlock()
		if client != nil {
			client.Close()
		}
	})

	first, err := d.fetchModels(false)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(first, []byte("gpt-5.6-sol")) {
		t.Fatalf("first catalog = %s", first)
	}

	fake.mu.Lock()
	fake.modelList = map[string]any{"data": []any{map[string]any{"model": "gpt-6-sol"}}}
	fake.mu.Unlock()
	d.mu.Lock()
	oldClient := d.client
	d.mu.Unlock()
	fake.dropConnections()
	<-oldClient.Done()

	// Between transports the last catalog is served at once, not a stalled
	// request or an error.
	if got, err := d.fetchModels(false); err != nil || !bytes.Contains(got, []byte("gpt-5.6-sol")) {
		t.Fatalf("catalog while disconnected = %s, err %v", got, err)
	}

	// Nano's session recovery reconnects the transport; the next fetch after
	// that must come from the new daemon.
	if err := d.ensure(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := d.fetchModels(false)
		if err == nil && bytes.Contains(got, []byte("gpt-6-sol")) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("catalog after reconnect = %s, err %v", got, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The real account upgrade added Ultrafast while Nano's transport stayed up.
// Boundary: the provider model catalog served by Nano's ordinary HTTP route.
func TestCodexModelCatalogRefreshDiscoversAccountUpgrade(t *testing.T) {
	fake := newFakeAppServer(t)
	fake.mu.Lock()
	fake.modelList = map[string]any{"data": []any{map[string]any{"model": "gpt-6-astra", "serviceTiers": []any{map[string]any{"id": "priority"}}}}}
	fake.mu.Unlock()
	d := fake.daemon()
	t.Cleanup(func() {
		d.mu.Lock()
		c := d.client
		d.mu.Unlock()
		if c != nil {
			c.Close()
		}
	})
	if _, err := d.fetchModels(false); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.modelList = map[string]any{"data": []any{map[string]any{"model": "gpt-6-astra", "serviceTiers": []any{map[string]any{"id": "ultrafast"}}}}}
	fake.mu.Unlock()
	s := &server{codex: d}
	rec := httptest.NewRecorder()
	s.handleCodexModels(rec, httptest.NewRequest(http.MethodGet, "/api/codex/models?refresh=true", nil))
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte("ultrafast")) {
		t.Fatalf("refreshed catalog = %d %s", rec.Code, rec.Body)
	}
}
