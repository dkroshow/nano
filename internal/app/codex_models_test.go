package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

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

// The real account upgrade added Ultrafast while Nano's transport stayed up.
// Boundary: the provider model catalog served by Nano's ordinary HTTP route.
func TestCodexModelCatalogRefreshDiscoversAccountUpgrade(t *testing.T) {
	fake := newFakeAppServer(t)
	fake.Mu.Lock()
	fake.ModelList = map[string]any{"data": []any{map[string]any{"model": "gpt-6-astra", "serviceTiers": []any{map[string]any{"id": "priority"}}}}}
	fake.Mu.Unlock()
	d := fake.daemon()
	t.Cleanup(fake.DropConnections)
	if _, err := d.FetchModels(false); err != nil {
		t.Fatal(err)
	}
	fake.Mu.Lock()
	fake.ModelList = map[string]any{"data": []any{map[string]any{"model": "gpt-6-astra", "serviceTiers": []any{map[string]any{"id": "ultrafast"}}}}}
	fake.Mu.Unlock()
	s := &server{codex: d}
	rec := httptest.NewRecorder()
	s.handleCodexModels(rec, httptest.NewRequest(http.MethodGet, "/api/codex/models?refresh=true", nil))
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte("ultrafast")) {
		t.Fatalf("refreshed catalog = %d %s", rec.Code, rec.Body)
	}
}
