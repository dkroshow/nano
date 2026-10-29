package hosted

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dkroshow/nano/internal/history"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/threadindex"
	ownedthreads "github.com/dkroshow/nano/internal/threads"
)

// PinCandidate is one indexed message offered as an explicit pin anchor.
type PinCandidate struct {
	ItemID, Role, Preview string
	At                    *string
}

// PinService owns pin admission, scoped reads and observation policy.
type PinService struct {
	Store                                    *PinStore
	Executions                               *hostedstore.ExecutionStore
	Catalog                                  PinCatalog
	CatalogErrors                            CatalogErrors
	CurrentNative                            func(string) (string, bool)
	ResolveHosted                            func(string) (ownedthreads.SessionRecord, bool)
	ResolveIdentity                          func(context.Context, string) (IdentityResolution, error)
	ResolveThread                            func(context.Context, string) (string, error)
	IsAmbiguous                              func(error) bool
	Transcripts                              threadindex.TranscriptCatalog
	WriteReferenceError                      func(http.ResponseWriter, error)
	WriteTranscriptError                     func(http.ResponseWriter, error)
	DefaultCandidateLimit, MaxCandidateLimit int
}

const (
	defaultPinContextTurns   = 10
	defaultPinListLimit      = 20
	maxPinListLimit          = 100
	maxPinTextBytes          = 500
	maxPinCommentaryBytes    = 4000
	maxPinCreatorBytes       = 200
	maxPinRequestBytes       = 16 << 10
	pinAnchorPreviewBytes    = 280
	maxPinMonitorAnchorCheck = 500
	pinContextResponseBudget = 256 << 10
)

var (
	errPinNotFound          = errors.New("pin not found")
	errPinIdempotencyReused = errors.New("clientActionId was already used for a different pin")
)

// PinCatalog supplies saved transcript anchors and bounded context.
type PinCatalog interface {
	LocatePinAnchor(ctx context.Context, threadID, itemID string) (history.PinMessage, error)
	ReadPinContext(ctx context.Context, threadID, itemID string, turns int) (history.PinContext, error)
}

type threadPinAnchorView struct {
	Provider       string `json:"provider"`
	NativeThreadID string `json:"nativeThreadId"`
	ItemID         string `json:"itemId"`
	TurnID         string `json:"turnId"`
	Role           string `json:"role"`
	At             string `json:"at,omitempty"`
	Preview        string `json:"preview"`
}

type threadPinCreatorView struct {
	Label          string `json:"label,omitempty"`
	DisplayName    string `json:"displayName,omitempty"`
	HostedThreadID string `json:"hostedThreadId,omitempty"`
	NativeThreadID string `json:"nativeThreadId,omitempty"`
}

type threadPinView struct {
	PinID          string                `json:"pinId"`
	HostedThreadID string                `json:"hostedThreadId"`
	Text           string                `json:"text"`
	Commentary     string                `json:"commentary,omitempty"`
	CreatedAt      string                `json:"createdAt"`
	Creator        *threadPinCreatorView `json:"creator,omitempty"`
	Anchor         threadPinAnchorView   `json:"anchor"`
	ContextURL     string                `json:"contextUrl"`
}

func pinRole(itemType string) string {
	switch itemType {
	case "user_message":
		return "user"
	case "agent_message":
		return "agent"
	}
	return itemType
}

// pinRequestOrigin reproduces the origin the caller used, so the generated
// link works from wherever the agent reached Nano.
func pinRequestOrigin(r *http.Request) string {
	host := strings.TrimSpace(r.Host)
	if host == "" {
		host = "127.0.0.1:7690"
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https") {
		scheme = "https"
	}
	return scheme + "://" + host
}

func pinContextPath(hostedThreadID, pinID string, turns int) string {
	return "/api/v1/hosted-threads/" + url.PathEscape(hostedThreadID) + "/pins/" + url.PathEscape(pinID) +
		"/context?turns=" + strconv.Itoa(turns)
}

// The context link names the hosted thread the caller asked through, so it
// works even when the pin was written under a predecessor hosted record.
func threadPinViewFor(r *http.Request, hostedThreadID string, pin threadPinRecord) threadPinView {
	view := threadPinView{
		PinID: pin.PinID, HostedThreadID: pin.HostedThreadID, Text: pin.Text, Commentary: pin.Commentary,
		CreatedAt: pin.CreatedAt,
		Anchor: threadPinAnchorView{
			Provider: pin.Provider, NativeThreadID: pin.NativeThreadID, ItemID: pin.AnchorItemID,
			TurnID: pin.AnchorTurnID, Role: pinRole(pin.AnchorType), At: pin.AnchorAt, Preview: pin.AnchorPreview,
		},
		ContextURL: pinRequestOrigin(r) + pinContextPath(hostedThreadID, pin.PinID, defaultPinContextTurns),
	}
	if pin.Creator != "" || pin.CreatorHostedThreadID != "" {
		view.Creator = &threadPinCreatorView{
			Label: pin.Creator, DisplayName: pin.CreatorDisplayName,
			HostedThreadID: pin.CreatorHostedThreadID, NativeThreadID: pin.CreatorNativeThreadID,
		}
	}
	return view
}

func (s *PinService) pinPreconditions(w http.ResponseWriter) (PinCatalog, bool) {
	w.Header().Set("Cache-Control", "private, no-store")
	if s.Store == nil {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "pins_unavailable", "Thread pins store unavailable")
		return nil, false
	}
	catalog := s.Catalog
	if catalog == nil {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
		return nil, false
	}
	return catalog, true
}

func (s *PinService) threadPinScopeFor(record ownedthreads.SessionRecord) threadPinScope {
	// The current native id always comes first: "pin this" anchors there.
	scope := threadPinScope{HostedThreadID: record.ID, Provider: record.Provider}
	if current, ok := s.CurrentNative(record.ID); ok && current != "" {
		scope.NativeIDs = append(scope.NativeIDs, current)
	}
	if ids, err := s.Executions.NativeIDsForHosted(record.ID); err == nil {
		for _, id := range ids {
			if len(scope.NativeIDs) == 0 || id != scope.NativeIDs[0] {
				scope.NativeIDs = append(scope.NativeIDs, id)
			}
		}
	}
	return scope
}

type threadPinCreateRequest struct {
	Text             string `json:"text"`
	Commentary       string `json:"commentary"`
	ItemID           string `json:"itemId"`
	Creator          string `json:"creator"`
	CreatorThreadRef string `json:"creatorThreadRef"`
	ClientActionID   string `json:"clientActionId"`
}

// HandleCreate appends an idempotent pin to a hosted transcript.
func (s *PinService) HandleCreate(w http.ResponseWriter, r *http.Request, record ownedthreads.SessionRecord) {
	catalog, ok := s.pinPreconditions(w)
	if !ok {
		return
	}
	var request threadPinCreateRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPinRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_pin", "Request body must be a JSON pin: "+err.Error())
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_pin", "Request body must contain one JSON object")
		return
	}
	request.Text = strings.TrimSpace(request.Text)
	request.Commentary = strings.TrimSpace(request.Commentary)
	request.Creator = strings.TrimSpace(request.Creator)
	request.ItemID = strings.TrimSpace(request.ItemID)
	request.ClientActionID = strings.TrimSpace(request.ClientActionID)
	switch {
	case request.Text == "":
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_pin", "text is required: a short phrase naming the topic to return to")
		return
	case len(request.Text) > maxPinTextBytes || !utf8.ValidString(request.Text):
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_pin", fmt.Sprintf("text must be valid UTF-8 of at most %d bytes; put detail in commentary", maxPinTextBytes))
		return
	case len(request.Commentary) > maxPinCommentaryBytes || !utf8.ValidString(request.Commentary):
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_pin", fmt.Sprintf("commentary must be valid UTF-8 of at most %d bytes", maxPinCommentaryBytes))
		return
	case len(request.Creator) > maxPinCreatorBytes || !utf8.ValidString(request.Creator):
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_pin", fmt.Sprintf("creator must be valid UTF-8 of at most %d bytes", maxPinCreatorBytes))
		return
	case len(request.ClientActionID) > 200:
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_pin", "clientActionId must be at most 200 bytes")
		return
	}

	pin := threadPinRecord{
		HostedThreadID: record.ID, Provider: record.Provider, Text: request.Text, Commentary: request.Commentary,
		Creator: request.Creator, ClientActionID: request.ClientActionID,
	}
	if ref := strings.TrimSpace(request.CreatorThreadRef); ref != "" {
		creator, ok := s.ResolveHosted(ref)
		if !ok {
			httpapi.WriteError(w, http.StatusBadRequest, "invalid_creator", "creatorThreadRef does not resolve to a hosted thread")
			return
		}
		pin.CreatorHostedThreadID, pin.CreatorNativeThreadID = creator.ID, creator.NativeThreadID
		if identity, err := s.ResolveIdentity(r.Context(), creator.ID); err == nil && identity.MatchCount == 1 {
			pin.CreatorDisplayName = identity.DisplayName
			if identity.NativeThreadID != "" {
				pin.CreatorNativeThreadID = identity.NativeThreadID
			}
		}
	}
	hash := sha256.Sum256([]byte(strings.Join([]string{record.ID, request.Text, request.Commentary, request.ItemID,
		request.Creator, pin.CreatorHostedThreadID}, "\x00")))
	pin.RequestHash = hex.EncodeToString(hash[:])
	if pin.ClientActionID != "" {
		// A retry must return the original pin even if "latest" has moved on.
		if existing, err := s.Store.byClientAction(record.ID, pin.ClientActionID); err == nil {
			if existing.RequestHash != pin.RequestHash {
				httpapi.WriteError(w, http.StatusConflict, "client_action_reused", errPinIdempotencyReused.Error())
				return
			}
			writeCatalogJSON(w, http.StatusOK, threadPinViewFor(r, record.ID, existing))
			return
		}
	}

	nativeID, anchor, err := s.locateThreadPinAnchor(r.Context(), catalog, record, request.ItemID)
	if err != nil {
		s.writeThreadPinAnchorError(w, err, request.ItemID)
		return
	}
	pin.NativeThreadID = nativeID
	pin.AnchorItemID, pin.AnchorTurnID, pin.AnchorType = anchor.ItemID, anchor.TurnKey, string(anchor.Type)
	if anchor.CreatedAt != nil {
		pin.AnchorAt = anchor.CreatedAt.UTC().Format(time.RFC3339Nano)
	}
	pin.AnchorPreview = truncateUTF8Bytes(strings.TrimSpace(anchor.Text), pinAnchorPreviewBytes)
	pin.PinID = newThreadPinID()
	pin.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	stored, created, err := s.Store.append(pin)
	if errors.Is(err, errPinIdempotencyReused) {
		httpapi.WriteError(w, http.StatusConflict, "client_action_reused", err.Error())
		return
	}
	if err != nil {
		httpapi.WriteError(w, http.StatusInternalServerError, "pin_error", "Pin write failed")
		return
	}
	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	writeCatalogJSON(w, status, threadPinViewFor(r, record.ID, stored))
}

var errPinThreadNotIndexed = errors.New("hosted thread has no indexed native transcript yet")

// locateThreadPinAnchor finds the anchor in the hosted thread's current
// native transcript, then in any earlier native transcript it has claimed, so
// "pin your earlier analysis" still works after a provider replacement. An
// empty itemID means the current conversational point: the latest message.
func (s *PinService) locateThreadPinAnchor(ctx context.Context, catalog PinCatalog, record ownedthreads.SessionRecord, itemID string) (string, history.PinMessage, error) {
	scope := s.threadPinScopeFor(record)
	if len(scope.NativeIDs) == 0 {
		return "", history.PinMessage{}, errPinThreadNotIndexed
	}
	candidates := scope.NativeIDs
	if itemID == "" {
		candidates = candidates[:1]
	}
	lastErr := error(errPinThreadNotIndexed)
	for _, nativeID := range candidates {
		threadID, err := s.ResolveThread(ctx, nativeID)
		if err != nil {
			continue
		}
		anchor, err := catalog.LocatePinAnchor(ctx, threadID, itemID)
		if err == nil {
			return nativeID, anchor, nil
		}
		if !errors.Is(err, history.ErrPinAnchorNotFound) && !errors.Is(err, s.CatalogErrors.NotFound) {
			return "", history.PinMessage{}, err
		}
		if errors.Is(err, history.ErrPinAnchorNotFound) {
			lastErr = err
		}
	}
	return "", history.PinMessage{}, lastErr
}

func (s *PinService) writeThreadPinAnchorError(w http.ResponseWriter, err error, itemID string) {
	switch {
	case errors.Is(err, errPinThreadNotIndexed):
		httpapi.WriteError(w, http.StatusConflict, "thread_not_indexed", "This hosted thread has no indexed transcript yet; retry after its first message is indexed")
	case errors.Is(err, history.ErrPinAnchorNotFound) && itemID == "":
		httpapi.WriteError(w, http.StatusConflict, "thread_not_indexed", "This hosted thread has no indexed user or agent message yet")
	case errors.Is(err, history.ErrPinAnchorNotFound):
		httpapi.WriteError(w, http.StatusNotFound, "anchor_not_found",
			"itemId is not a user or agent message in this thread; list candidates with GET /api/v1/hosted-threads/{threadRef}/pins/candidates or /api/v1/threads/{nativeThreadId}/messages?format=json")
	case errors.Is(err, history.ErrTranscriptConflict):
		httpapi.WriteError(w, http.StatusConflict, "anchor_conflict", "itemId resolves through a conflicting alias chain")
	default:
		s.CatalogErrors.writeBackendError(w, err)
	}
}

func truncateUTF8Bytes(value string, max int) string {
	if len(value) <= max {
		return value
	}
	value = value[:max]
	for !utf8.ValidString(value) && len(value) > 0 {
		value = value[:len(value)-1]
	}
	return value + "…"
}

type threadPinListResponse struct {
	HostedThreadID string          `json:"hostedThreadId"`
	Order          string          `json:"order"`
	Count          int             `json:"count"`
	Pins           []threadPinView `json:"pins"`
	HasMore        bool            `json:"hasMore"`
	NextCursor     *string         `json:"nextCursor"`
}

// HandleList lists hosted pins newest first.
func (s *PinService) HandleList(w http.ResponseWriter, r *http.Request, record ownedthreads.SessionRecord) {
	if _, ok := s.pinPreconditions(w); !ok {
		return
	}
	query := r.URL.Query()
	limit := defaultPinListLimit
	if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > maxPinListLimit {
			httpapi.WriteError(w, http.StatusBadRequest, "invalid_query", fmt.Sprintf("limit must be an integer from 1 to %d", maxPinListLimit))
			return
		}
		limit = value
	}
	var before int64
	if raw := strings.TrimSpace(query.Get("before")); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < 1 {
			httpapi.WriteError(w, http.StatusBadRequest, "invalid_cursor", "before must be a nextCursor value from an earlier page")
			return
		}
		before = value
	}
	scope := s.threadPinScopeFor(record)
	pins, more, err := s.Store.list(scope, before, limit)
	if err != nil {
		httpapi.WriteError(w, http.StatusInternalServerError, "pin_error", "Pin list failed")
		return
	}
	response := threadPinListResponse{HostedThreadID: record.ID, Order: "newest_first", Count: len(pins),
		Pins: make([]threadPinView, 0, len(pins)), HasMore: more}
	for _, pin := range pins {
		response.Pins = append(response.Pins, threadPinViewFor(r, record.ID, pin))
	}
	depth := 0
	if len(pins) > 0 {
		last := pins[len(pins)-1].Seq
		if more {
			cursor := strconv.FormatInt(last, 10)
			response.NextCursor = &cursor
		}
		depth, _ = s.Store.rank(scope, last)
	}
	s.Store.observeList(record.ID, before > 0, depth)
	writeCatalogJSON(w, http.StatusOK, response)
}

// HandleRead serves one pin in a hosted identity scope.
func (s *PinService) HandleRead(w http.ResponseWriter, r *http.Request, record ownedthreads.SessionRecord) {
	if _, ok := s.pinPreconditions(w); !ok {
		return
	}
	pin, err := s.Store.get(s.threadPinScopeFor(record), strings.TrimSpace(r.PathValue("pinId")))
	if err != nil {
		writeThreadPinLookupError(w, err)
		return
	}
	writeCatalogJSON(w, http.StatusOK, threadPinViewFor(r, record.ID, pin))
}

func writeThreadPinLookupError(w http.ResponseWriter, err error) {
	if errors.Is(err, errPinNotFound) {
		httpapi.WriteError(w, http.StatusNotFound, "pin_not_found", "No pin with that id belongs to this hosted thread")
		return
	}
	httpapi.WriteError(w, http.StatusInternalServerError, "pin_error", "Pin read failed")
}

// HandleCandidates lists recent messages so a caller can pick an exact anchor.
func (s *PinService) HandleCandidates(w http.ResponseWriter, r *http.Request, record ownedthreads.SessionRecord) {
	if _, ok := s.pinPreconditions(w); !ok {
		return
	}
	scope := s.threadPinScopeFor(record)
	if len(scope.NativeIDs) == 0 {
		httpapi.WriteError(w, http.StatusConflict, "thread_not_indexed", "This hosted thread has no indexed transcript yet")
		return
	}
	if s.Transcripts == nil {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
		return
	}
	tail := s.DefaultCandidateLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("tail")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > s.MaxCandidateLimit {
			httpapi.WriteError(w, http.StatusBadRequest, "invalid_query", fmt.Sprintf("tail must be an integer from 1 to %d", s.MaxCandidateLimit))
			return
		}
		tail = value
	}
	threadID, err := s.ResolveThread(r.Context(), scope.NativeIDs[0])
	if err != nil {
		s.WriteReferenceError(w, err)
		return
	}
	messages, err := s.readCandidates(r.Context(), threadID, tail)
	if err != nil {
		s.WriteTranscriptError(w, err)
		return
	}
	type candidate struct {
		ItemID  string  `json:"itemId"`
		Role    string  `json:"role"`
		At      *string `json:"at"`
		Preview string  `json:"preview"`
	}
	out := make([]candidate, 0, len(messages))
	for index := len(messages) - 1; index >= 0; index-- {
		message := messages[index]
		out = append(out, candidate{ItemID: message.ItemID, Role: message.Role, At: message.At,
			Preview: truncateUTF8Bytes(strings.TrimSpace(message.Preview), pinAnchorPreviewBytes)})
	}
	writeCatalogJSON(w, http.StatusOK, map[string]any{
		"hostedThreadId": record.ID, "nativeThreadId": scope.NativeIDs[0], "order": "newest_first", "candidates": out,
	})
}

func (s *PinService) readCandidates(ctx context.Context, id string, tail int) ([]PinCandidate, error) {
	messages, err := threadindex.CollectThreadMessages(ctx, s.Transcripts, id, threadindex.MessagesQuery{Tail: tail, Roles: map[string]bool{threadindex.MessageRoleUser: true, threadindex.MessageRoleAgent: true}})
	if err != nil {
		return nil, err
	}
	out := make([]PinCandidate, 0, len(messages))
	for _, message := range messages {
		out = append(out, PinCandidate{ItemID: message.ItemID, Role: message.Role, At: message.At, Preview: message.Preview})
	}
	return out, nil
}
