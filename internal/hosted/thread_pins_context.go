package hosted

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/httpapi"
)

type threadPinContextMessage struct {
	ItemID        string  `json:"itemId"`
	Role          string  `json:"role"`
	At            *string `json:"at"`
	Text          string  `json:"text"`
	Complete      bool    `json:"complete"`
	ContentState  string  `json:"contentState"`
	OriginalBytes int64   `json:"originalBytes"`
	IsAnchor      bool    `json:"isAnchor,omitempty"`
	preview       string
}

type threadPinContextTurn struct {
	TurnID       string                    `json:"turnId"`
	NativeTurnID string                    `json:"nativeTurnId,omitempty"`
	Ordinal      int                       `json:"ordinal"`
	Status       string                    `json:"status"`
	StartedAt    *string                   `json:"startedAt"`
	CompletedAt  *string                   `json:"completedAt"`
	IsAnchorTurn bool                      `json:"isAnchorTurn,omitempty"`
	StartsMid    bool                      `json:"startsMidTurn,omitempty"`
	Messages     []threadPinContextMessage `json:"messages"`
}

type threadPinContextResponse struct {
	Pin               threadPinView          `json:"pin"`
	CatalogThreadID   string                 `json:"catalogThreadId"`
	TurnsRequested    int                    `json:"turnsRequested"`
	TurnsReturned     int                    `json:"turnsReturned"`
	HasEarlierHistory bool                   `json:"hasEarlierHistory"`
	EarlierHistoryURL string                 `json:"earlierHistoryUrl,omitempty"`
	TrimmedForSize    bool                   `json:"trimmedForSize,omitempty"`
	Turns             []threadPinContextTurn `json:"turns"`
}

// HandleContext serves bounded saved context for one pin.
func (s *PinService) HandleContext(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
	catalog, ok := s.pinPreconditions(w)
	if !ok {
		return
	}
	turns := defaultPinContextTurns
	if raw := strings.TrimSpace(r.URL.Query().Get("turns")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 || value > history.MaxPinContextTurns {
			httpapi.WriteError(w, http.StatusBadRequest, "invalid_query", fmt.Sprintf("turns must be an integer from 0 to %d", history.MaxPinContextTurns))
			return
		}
		turns = value
	}
	format := strings.TrimSpace(r.URL.Query().Get("format"))
	if format != "" && format != "json" && format != "text" {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_query", "format must be json or text")
		return
	}
	scope := s.threadPinScopeFor(record)
	pin, err := s.Store.get(scope, strings.TrimSpace(r.PathValue("pinId")))
	if err != nil {
		writeThreadPinLookupError(w, err)
		return
	}
	rank, _ := s.Store.rank(scope, pin.Seq)
	view := threadPinViewFor(r, record.ID, pin)

	threadID, err := s.ResolveThread(r.Context(), pin.NativeThreadID)
	var pinContext history.PinContext
	if err == nil {
		pinContext, err = catalog.ReadPinContext(r.Context(), threadID, pin.AnchorItemID, turns)
	}
	if err != nil {
		code, message, status := "", "", http.StatusInternalServerError
		switch {
		case errors.Is(err, history.ErrPinAnchorNotFound):
			code, message, status = "pin_anchor_missing", "The anchored message is not in the current transcript index", http.StatusNotFound
		case errors.Is(err, s.CatalogErrors.NotFound):
			code, message, status = "pin_thread_not_indexed", "The anchored native thread is not in the transcript index", http.StatusNotFound
		}
		if code == "" {
			if s.IsAmbiguous(err) {
				code, message, status = "pin_thread_ambiguous", "The anchored native thread id matches several catalog threads", http.StatusConflict
			}
		}
		if code == "" {
			s.CatalogErrors.writeBackendError(w, err)
			return
		}
		// A broken anchor is reported, never repaired: the pin, its text, and
		// its commentary stay intact and are returned so the reader can still
		// use them.
		s.Store.observeContext(pin.PinID, rank, true)
		httpapi.WriteErrorEnvelope(w, status, httpapi.ErrorEnvelope{Error: httpapi.Error{Code: code, Message: message, Details: map[string]any{"pin": view}}})
		return
	}
	s.Store.observeContext(pin.PinID, rank, false)

	response := buildThreadPinContext(view, threadID, turns, pinContext)
	if format == "text" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, renderThreadPinContextText(response))
		return
	}
	writeCatalogJSON(w, http.StatusOK, response)
}

func formatPinTime(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := value.UTC().Format(time.RFC3339Nano)
	return &formatted
}

func buildThreadPinContext(view threadPinView, threadID string, requested int, source history.PinContext) threadPinContextResponse {
	response := threadPinContextResponse{
		Pin: view, CatalogThreadID: threadID, TurnsRequested: requested, HasEarlierHistory: source.HasEarlierHistory,
		Turns: make([]threadPinContextTurn, 0, len(source.Turns)),
	}
	index := map[string]int{}
	for _, turn := range source.Turns {
		index[turn.Key] = len(response.Turns)
		response.Turns = append(response.Turns, threadPinContextTurn{
			TurnID: turn.Key, NativeTurnID: turn.NativeTurnID, Ordinal: turn.Ordinal, Status: string(turn.Status),
			StartedAt: formatPinTime(turn.StartedAt), CompletedAt: formatPinTime(turn.CompletedAt),
			IsAnchorTurn: turn.Key == source.Anchor.TurnKey, Messages: []threadPinContextMessage{},
		})
	}
	for _, message := range source.Messages {
		position, ok := index[message.TurnKey]
		if !ok {
			continue
		}
		entry := threadPinContextMessage{
			ItemID: message.ItemID, Role: pinRole(string(message.Type)), At: formatPinTime(message.CreatedAt),
			Text: message.Text, Complete: message.ContentRef == "", ContentState: "inline",
			OriginalBytes: message.OriginalBytes, IsAnchor: message.ItemID == source.Anchor.ItemID, preview: message.Text,
		}
		if message.ContentRef != "" {
			entry.ContentState = "inline_preview"
			if message.Body != nil {
				entry.Text, entry.Complete = *message.Body, !message.BodyTruncated
				entry.ContentState = "retained_complete"
				if message.BodyTruncated {
					entry.ContentState = "retained_truncated_prefix"
				}
			}
		}
		response.Turns[position].Messages = append(response.Turns[position].Messages, entry)
	}
	trimThreadPinContext(&response)
	response.TurnsReturned = len(response.Turns)
	if response.TurnsReturned > 0 {
		response.TurnsReturned-- // the anchor's own turn is not a preceding turn
	}
	if response.HasEarlierHistory && len(response.Turns) > 0 {
		response.EarlierHistoryURL = "/api/v1/threads/" + url.PathEscape(view.Anchor.NativeThreadID) +
			"/transcript?beforeTurn=" + strconv.Itoa(response.Turns[0].Ordinal) + "&turnLimit=" + strconv.Itoa(defaultPinContextTurns)
	}
	return response
}

// trimThreadPinContext keeps the response inside the transcript budget. Full
// bodies fall back to their inline previews first (oldest first); then whole
// preceding turns are dropped (oldest first); only then are the anchor turn's
// earlier messages dropped. The anchor itself is always kept.
func trimThreadPinContext(response *threadPinContextResponse) {
	size := func() int {
		encoded, _ := json.Marshal(response)
		return len(encoded)
	}
	if size() <= pinContextResponseBudget {
		return
	}
	for t := range response.Turns {
		for m := range response.Turns[t].Messages {
			message := &response.Turns[t].Messages[m]
			if message.ContentState == "retained_complete" || message.ContentState == "retained_truncated_prefix" {
				message.Text, message.Complete, message.ContentState = message.preview, false, "inline_preview"
				response.TrimmedForSize = true
				if size() <= pinContextResponseBudget {
					return
				}
			}
		}
	}
	for len(response.Turns) > 1 && size() > pinContextResponseBudget {
		response.Turns = response.Turns[1:]
		response.HasEarlierHistory, response.TrimmedForSize = true, true
	}
	if len(response.Turns) == 0 {
		return
	}
	last := &response.Turns[len(response.Turns)-1]
	for len(last.Messages) > 1 && !last.Messages[0].IsAnchor && size() > pinContextResponseBudget {
		last.Messages = last.Messages[1:]
		last.StartsMid, response.HasEarlierHistory, response.TrimmedForSize = true, true, true
	}
}

func renderThreadPinContextText(response threadPinContextResponse) string {
	var out strings.Builder
	fmt.Fprintf(&out, "pin %s: %s\n", response.Pin.PinID, response.Pin.Text)
	if response.Pin.Commentary != "" {
		fmt.Fprintf(&out, "commentary: %s\n", response.Pin.Commentary)
	}
	fmt.Fprintf(&out, "anchor %s (%s) in %s/%s; %d preceding turns; earlier history: %t\n",
		response.Pin.Anchor.ItemID, response.Pin.Anchor.Role, response.Pin.Anchor.Provider,
		response.Pin.Anchor.NativeThreadID, response.TurnsReturned, response.HasEarlierHistory)
	for _, turn := range response.Turns {
		fmt.Fprintf(&out, "\n=== turn %d %s (%s)", turn.Ordinal, turn.TurnID, turn.Status)
		if turn.IsAnchorTurn {
			out.WriteString(" [anchor turn]")
		}
		if turn.StartsMid {
			out.WriteString(" [starts mid-turn]")
		}
		out.WriteString("\n")
		for _, message := range turn.Messages {
			out.WriteString(message.Role)
			if message.At != nil {
				out.WriteString(" [" + *message.At + "]")
			}
			out.WriteString(" item=" + message.ItemID)
			if message.IsAnchor {
				out.WriteString(" [PINNED]")
			}
			if !message.Complete {
				out.WriteString(" [" + message.ContentState + "]")
			}
			out.WriteString("\n")
			out.WriteString(strings.TrimRight(message.Text, "\n"))
			out.WriteString("\n\n")
		}
	}
	return out.String()
}
