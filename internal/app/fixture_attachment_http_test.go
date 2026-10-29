package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	hostedmodel "github.com/dkroshow/nano/internal/hosted"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/threads"
)

func fixtureSpawn(s *server, request hostedmodel.SpawnRequest) (*session, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	response := httptest.NewRecorder()
	fixtureOwners(s).hostedController().HandleLegacyCreate(response, httptest.NewRequest(http.MethodPost, "/api/sessions", bytes.NewReader(body)))
	if response.Code != http.StatusOK {
		return nil, fmt.Errorf("spawn HTTP %d: %s", response.Code, response.Body.String())
	}
	var summary sessionSummary
	if err := json.Unmarshal(response.Body.Bytes(), &summary); err != nil {
		return nil, err
	}
	return s.LookupSession(summary.ID), nil
}

// A conversation read uses the same synchronous boot-priority path as a pane.
func fixtureReattach(s *server, id string) {
	response := httptest.NewRecorder()
	fixtureOwners(s).hostedController().HandleHostedThreadConversation(response, httptest.NewRequest(http.MethodGet, "/conversation", nil), threads.SessionRecord{ID: id})
}

func fixtureAttachmentFor(s *server, provider, native string) (hostedstore.Attachment, bool) {
	if s.attachments == nil {
		return hostedstore.Attachment{}, false
	}
	return s.attachments.Get(provider, native)
}

func fixturePreviewArgs(t *testing.T, s *server, request hostedmodel.SpawnRequest) []string {
	t.Helper()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	fixtureOwners(s).attachmentCoordinator().HandleSpawnPreview(response, httptest.NewRequest(http.MethodPost, "/api/sessions/preview", bytes.NewReader(body)))
	if response.Code != http.StatusOK {
		t.Fatalf("preview HTTP %d: %s", response.Code, response.Body.String())
	}
	var result struct {
		Args []string `json:"argv"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result.Args
}
