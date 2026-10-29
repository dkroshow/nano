package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/observability"
)

func TestTranscriptRepairPriorityUsesFreshVisibleValidation(t *testing.T) {
	store := observability.NewValidationStore(10)
	handler := observability.NewBrowserHandlers(nil, store, nil, "")
	response := httptest.NewRecorder()
	handler.HandleTranscriptValidationRecord(response, httptest.NewRequest(http.MethodPost, "/api/v1/transcript-validations", strings.NewReader(`{"snapshotId":"tab:1","tabId":"tab","paneId":"pane","surface":"transcript","threadId":"native-open","status":"match","pageVisible":true,"surfaceVisible":true}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("validation post: %d %s", response.Code, response.Body.String())
	}
	server := testServerWithSessions(&server{transcriptValidations: store, Registry: newSessionRegistry()}, map[string]*session{
		"attached": newFixtureSession(sessionConfig{ID: "attached", Provider: "codex"}, "native-attached"),
	})
	if got := server.attachmentCoordinator().TranscriptRepairPriority(history.TranscriptParserBackfillCandidate{Provider: history.ProviderCodex, NativeThreadID: "native-open"}); got != 0 {
		t.Fatalf("open priority=%d", got)
	}
	if got := server.attachmentCoordinator().TranscriptRepairPriority(history.TranscriptParserBackfillCandidate{Provider: history.ProviderCodex, NativeThreadID: "native-attached"}); got != 1 {
		t.Fatalf("attached priority=%d", got)
	}
	if got := server.attachmentCoordinator().TranscriptRepairPriority(history.TranscriptParserBackfillCandidate{Provider: history.ProviderCodex, NativeThreadID: "native-other"}); got != 2 {
		t.Fatalf("ordinary priority=%d", got)
	}
}
