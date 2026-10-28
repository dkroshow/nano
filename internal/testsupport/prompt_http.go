package testsupport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/core/prompts"
)

// PromptHTTP is the attachment's delivery HTTP boundary exercised by fixtures.
type PromptHTTP interface {
	HandleTurn(http.ResponseWriter, *http.Request)
	HandleTurnDeliveryRead(http.ResponseWriter, *http.Request, string)
}

func PostTurn(t *testing.T, sess PromptHTTP, ctx context.Context, actionID, text string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"text":` + string(JSONString(t, text)) + `,"clientActionId":` + string(JSONString(t, actionID)) + `}`
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/nano-session/turn", strings.NewReader(body)).WithContext(ctx)
	rec := httptest.NewRecorder()
	sess.HandleTurn(rec, req)
	return rec
}

// ReadPrompt reads one prompt through the delivery read route.
func ReadPrompt(t *testing.T, sess PromptHTTP, actionID string) (prompts.View, int) {
	t.Helper()
	rec := httptest.NewRecorder()
	sess.HandleTurnDeliveryRead(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/nano-session/turn-delivery/"+actionID, nil), actionID)
	var view prompts.View
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
	}
	return view, rec.Code
}

func JSONString(t *testing.T, value string) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
