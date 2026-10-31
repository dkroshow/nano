package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	hostedmodel "github.com/dkroshow/nano/internal/hosted"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	state "github.com/dkroshow/nano/internal/state/store"
	"github.com/dkroshow/nano/internal/threads"
)

type bulkCloseMemberResult struct {
	HostedThreadID string `json:"hostedThreadId"`
	Provider       string `json:"provider"`
	NativeThreadID string `json:"nativeThreadId,omitempty"`
	PreviousStatus string `json:"previousStatus"`
	FinalStatus    string `json:"finalStatus"`
	Working        bool   `json:"working"`
	CloseAction    string `json:"closeAction"`
	Error          string `json:"error,omitempty"`
}
type bulkCloseResponse struct {
	RootHostedThreadID string                  `json:"rootHostedThreadId"`
	Complete           bool                    `json:"complete"`
	MemberCount        int                     `json:"memberCount"`
	WorkingCount       int                     `json:"workingCount"`
	Members            []bulkCloseMemberResult `json:"members"`
}
type bulkClosePreviewMember struct {
	HostedThreadID string `json:"hostedThreadId"`
	Provider       string `json:"provider"`
	NativeThreadID string `json:"nativeThreadId,omitempty"`
	Status         string `json:"status"`
	Depth          int    `json:"depth"`
	Working        bool   `json:"working"`
}
type bulkClosePreview struct {
	RootHostedThreadID string                   `json:"rootHostedThreadId"`
	MemberCount        int                      `json:"memberCount"`
	WorkingCount       int                      `json:"workingCount"`
	Members            []bulkClosePreviewMember `json:"members"`
}

func senderPresentationName(a hostedstore.TurnAttribution) string {
	if name := strings.TrimSpace(a.SenderName); name != "" {
		return name
	}
	provider, native := strings.TrimSpace(a.SenderProvider), strings.TrimSpace(a.SenderNativeThreadID)
	if provider == "" || native == "" {
		return ""
	}
	prefix := native
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	return provider + " " + prefix + " (unnamed)"
}

// senderFooter is the one trailing line Nano appends to every agent-origin
// prompt so it never reads as a human message. Agent footers identify the
// sender and Nano's original UTC admission time, before any queue delay.
// Agent submissions reach this function only after complete attribution
// admission. System prompts retain their distinct Nano footer.
func senderFooter(a hostedstore.TurnAttribution) string {
	if a.Origin == turnOriginSystem {
		return "[from nano]"
	}
	name := senderPresentationName(a)
	native := strings.TrimSpace(a.SenderNativeThreadID)
	submittedAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(a.SenderSubmittedAt))
	if name == "" || native == "" || err != nil {
		return ""
	}
	return "[from " + name + " · " + native + " · submitted " + submittedAt.UTC().Format(time.RFC3339) + "]"
}
func hostedTurnPromptHash(prompt string, requireIdle bool) string {
	if !requireIdle {
		return sendTextHash(prompt)
	}
	return sendTextHash(prompt + "\x00requireIdle")
}

func (s *server) submitHostedTurnWithOptions(id, action, prompt string, idle bool) (threads.TurnRecord, bool, error) {
	origin := turnOriginSystem
	if !strings.HasPrefix(action, "nano:") {
		origin = turnOriginAgent
	}
	raw, err := json.Marshal(map[string]any{"clientActionId": action, "prompt": prompt, "origin": origin, "requireIdle": idle})
	if err != nil {
		return threads.TurnRecord{}, false, err
	}
	response := httptest.NewRecorder()
	fixtureOwners(s).hostedController().HandleTurnSubmit(response, httptest.NewRequest(http.MethodPost, "/api/v1/hosted-threads/"+id+"/turns", strings.NewReader(string(raw))), id)
	if response.Code >= 400 {
		return threads.TurnRecord{}, false, errors.New(response.Body.String())
	}
	var view hostedmodel.TurnView
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		return threads.TurnRecord{}, false, err
	}
	record, _ := s.agentExecutions.Turn(view.ID)
	return record, response.Code == http.StatusOK, nil
}

type retiredHostedReceipt struct {
	state.RetirementReceipt
	OK          bool                             `json:"ok"`
	Error       string                           `json:"error,omitempty"`
	Resolutions []hostedmodel.IdentityResolution `json:"resolutions"`
}

func (s *server) retireHostedRecords(ctx context.Context, request state.RetirementRequest) retiredHostedReceipt {
	raw, err := json.Marshal(request)
	if err != nil {
		return retiredHostedReceipt{Error: err.Error()}
	}
	response := httptest.NewRecorder()
	fixtureOwners(s).hostedController().HandleRetirement(response, httptest.NewRequest(http.MethodPost, "/api/v1/maintenance/retired-hosted", strings.NewReader(string(raw))).WithContext(ctx))
	var receipt retiredHostedReceipt
	if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil {
		receipt.Error = err.Error()
	}
	return receipt
}
