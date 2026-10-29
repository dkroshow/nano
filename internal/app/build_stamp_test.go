package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dkroshow/nano/internal/core/prompts"
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
	if body.StoreSchema != prompts.TurnSchemaVersion {
		t.Fatalf("storeSchema = %d, want %d", body.StoreSchema, prompts.TurnSchemaVersion)
	}
}
