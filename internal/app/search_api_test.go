package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/threadindex"
)

type fakeSearchCatalog struct {
	*catalogRowsFixture
	page     history.ConversationSearchPage
	statuses []history.ConversationSearchStatus
	query    history.ConversationSearchQuery
	err      error
}

func (f *fakeSearchCatalog) SearchConversationItems(_ context.Context, query history.ConversationSearchQuery) (history.ConversationSearchPage, error) {
	f.query = query
	return f.page, f.err
}
func (f *fakeSearchCatalog) ConversationSearchStatus(context.Context) ([]history.ConversationSearchStatus, error) {
	return f.statuses, f.err
}

func TestConversationSearchHandlersValidateAndMapContractErrors(t *testing.T) {
	fake := &fakeSearchCatalog{catalogRowsFixture: &catalogRowsFixture{}, page: history.ConversationSearchPage{Mode: history.ConversationSearchLiteral, Group: "message", SearchRevision: 7, Items: []history.ConversationSearchItem{}}}
	handler := threadindex.NewHandlers(fake)
	rec := httptest.NewRecorder()
	handler.HandleConversationSearchMessages(rec, httptest.NewRequest(http.MethodGet, "/api/v1/search/messages?q=alpha&mode=literal&role=user&role=assistant&provider=codex&provider=claude&kind=main&kind=subagent&limit=3", nil))
	if rec.Code != http.StatusOK || fake.query.Query != "alpha" || fake.query.Limit != 3 || len(fake.query.Roles) != 2 || len(fake.query.Providers) != 2 || len(fake.query.Kinds) != 2 {
		t.Fatalf("search handler = %d %#v %s", rec.Code, fake.query, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	handler.HandleConversationSearchMessages(rec, httptest.NewRequest(http.MethodGet, "/api/v1/search/messages?q=ab&mode=literal", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid query = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	handler.HandleConversationSearchMessages(rec, httptest.NewRequest(http.MethodGet, "/api/v1/search/messages?q=alpha&mode=literal&role=system", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unsupported role = %d", rec.Code)
	}
	fake.err = history.ErrConversationSearchInvalidCursor
	rec = httptest.NewRecorder()
	handler.HandleConversationSearchMessages(rec, httptest.NewRequest(http.MethodGet, "/api/v1/search/messages?q=alpha&mode=literal&cursor=bad", nil))
	var errorBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if rec.Code != http.StatusBadRequest || json.Unmarshal(rec.Body.Bytes(), &errorBody) != nil || errorBody.Error.Code != "invalid_cursor" {
		t.Fatalf("tampered cursor = %d %s", rec.Code, rec.Body.String())
	}
	fake.err = history.ErrConversationSearchChanged
	rec = httptest.NewRecorder()
	handler.HandleConversationSearchMessages(rec, httptest.NewRequest(http.MethodGet, "/api/v1/search/messages?q=alpha&mode=literal&cursor=valid", nil))
	if rec.Code != http.StatusConflict || json.Unmarshal(rec.Body.Bytes(), &errorBody) != nil || errorBody.Error.Code != "search_changed" {
		t.Fatalf("stale cursor = %d %s", rec.Code, rec.Body.String())
	}
	fake.err = &history.ConversationSearchUnavailableError{Status: history.ConversationSearchStatus{Mode: history.ConversationSearchLiteral, State: history.ConversationSearchBuilding}}
	rec = httptest.NewRecorder()
	handler.HandleConversationSearchMessages(rec, httptest.NewRequest(http.MethodGet, "/api/v1/search/messages?q=alpha&mode=literal", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("building = %d", rec.Code)
	}
	fake.err = nil
	fake.statuses = []history.ConversationSearchStatus{{Mode: history.ConversationSearchLiteral, State: history.ConversationSearchReady}}
	rec = httptest.NewRecorder()
	handler.HandleConversationSearchStatus(rec, httptest.NewRequest(http.MethodGet, "/api/v1/search/status", nil))
	var body struct {
		Modes []history.ConversationSearchStatus `json:"modes"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil || len(body.Modes) != 1 {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	if !errors.Is(fake.err, nil) {
		t.Fatal("unexpected fake error")
	}
}
