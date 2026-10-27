package app

import (
	"encoding/json"
	turns "github.com/dkroshow/nano/internal/core/prompts"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBuildInfoPublishesSupportedThreadsSchema(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/api/v1/build", nil)
	response := httptest.NewRecorder()

	handleBuildInfo(response, request)

	var body struct {
		StoreSchema int `json:"storeSchema"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.StoreSchema != turns.TurnSchemaVersion {
		t.Fatalf("storeSchema = %d, want %d", body.StoreSchema, turns.TurnSchemaVersion)
	}
}
