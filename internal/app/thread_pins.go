package app

// thread_pins.go — thread-local transcript pins.
//
// A pin is a lightweight bookmark an agent appends to its own hosted thread so
// a later turn can return to a paused topic. Pins are append-only and
// listed newest-first; an agent reads backward until it finds the topic.
//
// Each pin anchors one normalized user or agent message by provider, native
// thread id, and canonical item id. Canonical ids are deterministic, so the
// anchor survives a delete-and-rebuild of history.sqlite. Pins themselves are
// agent-authored truth, so they live in a sibling SQLite file
// (~/.nanod/thread-pins.sqlite, flag -thread-pins-db), like stars.
//
// Pins attach to the hosted thread id. A thread's pin list also includes pins
// anchored in any native id that hosted thread has ever claimed, so provider
// replacement or reattachment never strands them. The context link is
// generated from the anchor at read time; no absolute URL is stored.
//
// Nothing here injects text into a turn. An agent reads pins only by calling
// the API, so every byte it sees appears in its own native transcript.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/httpapi"
	statedb "github.com/dkroshow/nano/internal/state/db"
)

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
	pinContextResponseBudget = maxThreadTranscriptBytes
)

var (
	errPinNotFound          = errors.New("pin not found")
	errPinIdempotencyReused = errors.New("clientActionId was already used for a different pin")
)

type threadPinsStore struct {
	db *sql.DB
	mu sync.Mutex
}

func openThreadPinsStore(path string) (*threadPinsStore, error) {
	path = strings.TrimSpace(path)
	absolute, err := canonicalProspectivePath(path)
	if err != nil {
		return nil, fmt.Errorf("resolve thread pins database: %w", err)
	}
	db, err := statedb.Open(context.Background(), absolute)
	if err != nil {
		return nil, err
	}
	// The observation tables belong to the monitor. They count reads; they
	// never reference pins by foreign key and never change a pin row.
	if _, err = db.Exec(`
CREATE TABLE IF NOT EXISTS thread_pins (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  pin_id TEXT NOT NULL UNIQUE,
  hosted_thread_id TEXT NOT NULL,
  provider TEXT NOT NULL,
  native_thread_id TEXT NOT NULL,
  anchor_item_id TEXT NOT NULL,
  anchor_turn_id TEXT NOT NULL,
  anchor_type TEXT NOT NULL,
  anchor_at TEXT,
  anchor_preview TEXT NOT NULL,
  text TEXT NOT NULL,
  commentary TEXT NOT NULL DEFAULT '',
  creator TEXT NOT NULL DEFAULT '',
  creator_hosted_thread_id TEXT NOT NULL DEFAULT '',
  creator_native_thread_id TEXT NOT NULL DEFAULT '',
  creator_display_name TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  client_action_id TEXT,
  request_hash TEXT NOT NULL DEFAULT '',
  UNIQUE(hosted_thread_id, client_action_id)
);
CREATE INDEX IF NOT EXISTS thread_pins_hosted ON thread_pins(hosted_thread_id, seq DESC);
CREATE INDEX IF NOT EXISTS thread_pins_native ON thread_pins(provider, native_thread_id, seq DESC);
CREATE TABLE IF NOT EXISTS thread_pin_context_reads (
  pin_id TEXT PRIMARY KEY,
  reads INTEGER NOT NULL,
  broken_reads INTEGER NOT NULL,
  rank_total INTEGER NOT NULL,
  max_rank INTEGER NOT NULL,
  first_read_at TEXT NOT NULL,
  last_read_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS thread_pin_list_reads (
  hosted_thread_id TEXT PRIMARY KEY,
  calls INTEGER NOT NULL,
  continued_calls INTEGER NOT NULL,
  max_depth INTEGER NOT NULL,
  last_read_at TEXT NOT NULL
)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate thread pins database: %w", err)
	}
	return &threadPinsStore{db: db}, nil
}

func (s *threadPinsStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

type threadPinRecord struct {
	Seq                   int64
	PinID                 string
	HostedThreadID        string
	Provider              string
	NativeThreadID        string
	AnchorItemID          string
	AnchorTurnID          string
	AnchorType            string
	AnchorAt              string
	AnchorPreview         string
	Text                  string
	Commentary            string
	Creator               string
	CreatorHostedThreadID string
	CreatorNativeThreadID string
	CreatorDisplayName    string
	CreatedAt             string
	ClientActionID        string
	RequestHash           string
}

const threadPinColumns = `seq, pin_id, hosted_thread_id, provider, native_thread_id, anchor_item_id,
anchor_turn_id, anchor_type, COALESCE(anchor_at, ''), anchor_preview, text, commentary, creator,
creator_hosted_thread_id, creator_native_thread_id, creator_display_name, created_at,
COALESCE(client_action_id, ''), request_hash`

func scanThreadPin(row interface{ Scan(...any) error }) (threadPinRecord, error) {
	var p threadPinRecord
	err := row.Scan(&p.Seq, &p.PinID, &p.HostedThreadID, &p.Provider, &p.NativeThreadID, &p.AnchorItemID,
		&p.AnchorTurnID, &p.AnchorType, &p.AnchorAt, &p.AnchorPreview, &p.Text, &p.Commentary, &p.Creator,
		&p.CreatorHostedThreadID, &p.CreatorNativeThreadID, &p.CreatorDisplayName, &p.CreatedAt,
		&p.ClientActionID, &p.RequestHash)
	return p, err
}

// append inserts a new pin. A retried clientActionId with the same request
// returns the original pin; with a different request it is refused.
func (s *threadPinsStore) append(pin threadPinRecord) (threadPinRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if pin.ClientActionID != "" {
		existing, err := s.byClientActionLocked(pin.HostedThreadID, pin.ClientActionID)
		if err == nil {
			if existing.RequestHash != pin.RequestHash {
				return threadPinRecord{}, false, errPinIdempotencyReused
			}
			return existing, false, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return threadPinRecord{}, false, err
		}
	}
	var clientActionID any
	if pin.ClientActionID != "" {
		clientActionID = pin.ClientActionID
	}
	result, err := s.db.Exec(`INSERT INTO thread_pins(pin_id, hosted_thread_id, provider, native_thread_id,
anchor_item_id, anchor_turn_id, anchor_type, anchor_at, anchor_preview, text, commentary, creator,
creator_hosted_thread_id, creator_native_thread_id, creator_display_name, created_at, client_action_id, request_hash)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		pin.PinID, pin.HostedThreadID, pin.Provider, pin.NativeThreadID, pin.AnchorItemID, pin.AnchorTurnID,
		pin.AnchorType, pinNullable(pin.AnchorAt), pin.AnchorPreview, pin.Text, pin.Commentary, pin.Creator,
		pin.CreatorHostedThreadID, pin.CreatorNativeThreadID, pin.CreatorDisplayName, pin.CreatedAt,
		clientActionID, pin.RequestHash)
	if err != nil {
		return threadPinRecord{}, false, err
	}
	pin.Seq, _ = result.LastInsertId()
	return pin, true, nil
}

func pinNullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// threadPinScope is the set of pins that belong to one hosted thread: pins
// written under its hosted id, plus pins anchored in any native id it has ever
// claimed.
type threadPinScope struct {
	HostedThreadID string
	Provider       string
	NativeIDs      []string
}

func (scope threadPinScope) where() (string, []any) {
	clause := `(hosted_thread_id = ?`
	args := []any{scope.HostedThreadID}
	if len(scope.NativeIDs) > 0 {
		clause += ` OR (provider = ? AND native_thread_id IN (?` + strings.Repeat(`,?`, len(scope.NativeIDs)-1) + `))`
		args = append(args, scope.Provider)
		for _, id := range scope.NativeIDs {
			args = append(args, id)
		}
	}
	return clause + `)`, args
}

// list returns up to limit pins older than beforeSeq (0 = newest), newest
// first, and whether more exist.
func (s *threadPinsStore) list(scope threadPinScope, beforeSeq int64, limit int) ([]threadPinRecord, bool, error) {
	where, args := scope.where()
	if beforeSeq > 0 {
		where += ` AND seq < ?`
		args = append(args, beforeSeq)
	}
	args = append(args, limit+1)
	rows, err := s.db.Query(`SELECT `+threadPinColumns+` FROM thread_pins WHERE `+where+` ORDER BY seq DESC LIMIT ?`, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	pins := make([]threadPinRecord, 0, limit)
	for rows.Next() {
		pin, err := scanThreadPin(rows)
		if err != nil {
			return nil, false, err
		}
		pins = append(pins, pin)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(pins) > limit
	if more {
		pins = pins[:limit]
	}
	return pins, more, nil
}

func (s *threadPinsStore) get(scope threadPinScope, pinID string) (threadPinRecord, error) {
	where, args := scope.where()
	args = append(args, pinID)
	pin, err := scanThreadPin(s.db.QueryRow(`SELECT `+threadPinColumns+` FROM thread_pins WHERE `+where+` AND pin_id = ?`, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return threadPinRecord{}, errPinNotFound
	}
	return pin, err
}

// rank is the pin's 1-based newest-first position within its scope.
func (s *threadPinsStore) rank(scope threadPinScope, seq int64) (int, error) {
	where, args := scope.where()
	args = append(args, seq)
	var rank int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM thread_pins WHERE `+where+` AND seq >= ?`, args...).Scan(&rank)
	return rank, err
}

// Observation writes are best-effort: a failed count must never fail the read
// that an agent is relying on.
func (s *threadPinsStore) observeList(hostedThreadID string, continued bool, depth int) {
	cont := 0
	if continued {
		cont = 1
	}
	_, _ = s.db.Exec(`INSERT INTO thread_pin_list_reads(hosted_thread_id, calls, continued_calls, max_depth, last_read_at)
VALUES (?, 1, ?, ?, ?)
ON CONFLICT(hosted_thread_id) DO UPDATE SET calls = calls + 1, continued_calls = continued_calls + excluded.continued_calls,
  max_depth = MAX(max_depth, excluded.max_depth), last_read_at = excluded.last_read_at`,
		hostedThreadID, cont, depth, time.Now().UTC().Format(time.RFC3339Nano))
}

func (s *threadPinsStore) observeContext(pinID string, rank int, broken bool) {
	brokenCount := 0
	if broken {
		brokenCount = 1
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = s.db.Exec(`INSERT INTO thread_pin_context_reads(pin_id, reads, broken_reads, rank_total, max_rank, first_read_at, last_read_at)
VALUES (?, 1, ?, ?, ?, ?, ?)
ON CONFLICT(pin_id) DO UPDATE SET reads = reads + 1, broken_reads = broken_reads + excluded.broken_reads,
  rank_total = rank_total + excluded.rank_total, max_rank = MAX(max_rank, excluded.max_rank),
  last_read_at = excluded.last_read_at`, pinID, brokenCount, rank, rank, now, now)
}

// ---- catalog seam -------------------------------------------------------

type threadPinCatalog interface {
	LocatePinAnchor(ctx context.Context, threadID, itemID string) (history.PinMessage, error)
	ReadPinContext(ctx context.Context, threadID, itemID string, turns int) (history.PinContext, error)
}

func (s *threadCatalogService) LocatePinAnchor(ctx context.Context, threadID, itemID string) (history.PinMessage, error) {
	message, err := s.store.LocatePinAnchor(ctx, threadID, itemID)
	return message, mapPinCatalogError(err)
}

func (s *threadCatalogService) ReadPinContext(ctx context.Context, threadID, itemID string, turns int) (history.PinContext, error) {
	result, err := s.store.ReadPinContext(ctx, threadID, itemID, turns)
	return result, mapPinCatalogError(err)
}

func mapPinCatalogError(err error) error {
	if errors.Is(err, history.ErrThreadNotFound) {
		return errCatalogThreadNotFound
	}
	return err
}

// ---- views --------------------------------------------------------------

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
	if role, ok := threadMessageRole(itemType); ok {
		return role
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

// ---- handlers -----------------------------------------------------------

func (s *server) pinPreconditions(w http.ResponseWriter) (threadPinCatalog, bool) {
	w.Header().Set("Cache-Control", "private, no-store")
	if s.threadPins == nil {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "pins_unavailable", "Thread pins store unavailable")
		return nil, false
	}
	catalog, ok := s.catalog.(threadPinCatalog)
	if s.catalog == nil || !ok {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
		return nil, false
	}
	return catalog, true
}

func (s *server) threadPinScopeFor(record agentSessionRecord) threadPinScope {
	// The current native id always comes first: "pin this" anchors there.
	scope := threadPinScope{HostedThreadID: record.ID, Provider: record.Provider}
	if current, ok := s.hostedThreadNativeID(record.ID); ok && current != "" {
		scope.NativeIDs = append(scope.NativeIDs, current)
	}
	if ids, err := s.agentExecutions.nativeIDsForHosted(record.ID); err == nil {
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

func (s *server) handleThreadPinCreate(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
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
		creator, ok := s.resolveHostedThreadRecord(ref)
		if !ok {
			httpapi.WriteError(w, http.StatusBadRequest, "invalid_creator", "creatorThreadRef does not resolve to a hosted thread")
			return
		}
		pin.CreatorHostedThreadID, pin.CreatorNativeThreadID = creator.ID, creator.NativeThreadID
		if identity, err := s.resolveIdentity(r.Context(), creator.ID); err == nil && identity.MatchCount == 1 {
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
		if existing, err := s.threadPins.byClientAction(record.ID, pin.ClientActionID); err == nil {
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
		writeThreadPinAnchorError(w, err, request.ItemID)
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
	stored, created, err := s.threadPins.append(pin)
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

// clientActionId is scoped to one hosted thread: two threads may reuse a key.
func (s *threadPinsStore) byClientAction(hostedThreadID, clientActionID string) (threadPinRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byClientActionLocked(hostedThreadID, clientActionID)
}

func (s *threadPinsStore) byClientActionLocked(hostedThreadID, clientActionID string) (threadPinRecord, error) {
	return scanThreadPin(s.db.QueryRow(`SELECT `+threadPinColumns+` FROM thread_pins WHERE hosted_thread_id = ? AND client_action_id = ?`,
		hostedThreadID, clientActionID))
}

var errPinThreadNotIndexed = errors.New("hosted thread has no indexed native transcript yet")

// locateThreadPinAnchor finds the anchor in the hosted thread's current
// native transcript, then in any earlier native transcript it has claimed, so
// "pin your earlier analysis" still works after a provider replacement. An
// empty itemID means the current conversational point: the latest message.
func (s *server) locateThreadPinAnchor(ctx context.Context, catalog threadPinCatalog, record agentSessionRecord, itemID string) (string, history.PinMessage, error) {
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
		threadID, err := s.resolveThreadReference(ctx, nativeID)
		if err != nil {
			continue
		}
		anchor, err := catalog.LocatePinAnchor(ctx, threadID, itemID)
		if err == nil {
			return nativeID, anchor, nil
		}
		if !errors.Is(err, history.ErrPinAnchorNotFound) && !errors.Is(err, errCatalogThreadNotFound) {
			return "", history.PinMessage{}, err
		}
		if errors.Is(err, history.ErrPinAnchorNotFound) {
			lastErr = err
		}
	}
	return "", history.PinMessage{}, lastErr
}

func writeThreadPinAnchorError(w http.ResponseWriter, err error, itemID string) {
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
		writeCatalogBackendError(w, err)
	}
}

func newThreadPinID() string {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(err)
	}
	return "pin_" + hex.EncodeToString(raw[:])
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

func (s *server) handleThreadPinList(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
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
	pins, more, err := s.threadPins.list(scope, before, limit)
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
		depth, _ = s.threadPins.rank(scope, last)
	}
	s.threadPins.observeList(record.ID, before > 0, depth)
	writeCatalogJSON(w, http.StatusOK, response)
}

func (s *server) handleThreadPinRead(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
	if _, ok := s.pinPreconditions(w); !ok {
		return
	}
	pin, err := s.threadPins.get(s.threadPinScopeFor(record), strings.TrimSpace(r.PathValue("pinId")))
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

// ---- context ------------------------------------------------------------

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

func (s *server) handleThreadPinContext(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
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
	if format != "" && format != threadMessagesFormatJSON && format != threadMessagesFormatText {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_query", "format must be json or text")
		return
	}
	scope := s.threadPinScopeFor(record)
	pin, err := s.threadPins.get(scope, strings.TrimSpace(r.PathValue("pinId")))
	if err != nil {
		writeThreadPinLookupError(w, err)
		return
	}
	rank, _ := s.threadPins.rank(scope, pin.Seq)
	view := threadPinViewFor(r, record.ID, pin)

	threadID, err := s.resolveThreadReference(r.Context(), pin.NativeThreadID)
	var pinContext history.PinContext
	if err == nil {
		pinContext, err = catalog.ReadPinContext(r.Context(), threadID, pin.AnchorItemID, turns)
	}
	if err != nil {
		code, message, status := "", "", http.StatusInternalServerError
		switch {
		case errors.Is(err, history.ErrPinAnchorNotFound):
			code, message, status = "pin_anchor_missing", "The anchored message is not in the current transcript index", http.StatusNotFound
		case errors.Is(err, errCatalogThreadNotFound):
			code, message, status = "pin_thread_not_indexed", "The anchored native thread is not in the transcript index", http.StatusNotFound
		}
		if code == "" {
			var ambiguous *ambiguousThreadReferenceError
			if errors.As(err, &ambiguous) {
				code, message, status = "pin_thread_ambiguous", "The anchored native thread id matches several catalog threads", http.StatusConflict
			}
		}
		if code == "" {
			writeCatalogBackendError(w, err)
			return
		}
		// A broken anchor is reported, never repaired: the pin, its text, and
		// its commentary stay intact and are returned so the reader can still
		// use them.
		s.threadPins.observeContext(pin.PinID, rank, true)
		httpapi.WriteErrorEnvelope(w, status, httpapi.ErrorEnvelope{Error: httpapi.Error{Code: code, Message: message, Details: map[string]any{"pin": view}}})
		return
	}
	s.threadPins.observeContext(pin.PinID, rank, false)

	response := buildThreadPinContext(view, threadID, turns, pinContext)
	if format == threadMessagesFormatText {
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

// ---- candidates ---------------------------------------------------------

// handleThreadPinCandidates lists recent messages with their item ids, so an
// agent asked to "pin your earlier analysis" can pick the exact message.
func (s *server) handleThreadPinCandidates(w http.ResponseWriter, r *http.Request, record agentSessionRecord) {
	if _, ok := s.pinPreconditions(w); !ok {
		return
	}
	scope := s.threadPinScopeFor(record)
	if len(scope.NativeIDs) == 0 {
		httpapi.WriteError(w, http.StatusConflict, "thread_not_indexed", "This hosted thread has no indexed transcript yet")
		return
	}
	catalog, ok := s.catalog.(threadTranscriptCatalog)
	if !ok {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
		return
	}
	query := threadMessagesQuery{Tail: defaultThreadMessagesTail,
		Roles: map[string]bool{threadMessageRoleUser: true, threadMessageRoleAgent: true}}
	if raw := strings.TrimSpace(r.URL.Query().Get("tail")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > maxThreadMessagesTail {
			httpapi.WriteError(w, http.StatusBadRequest, "invalid_query", fmt.Sprintf("tail must be an integer from 1 to %d", maxThreadMessagesTail))
			return
		}
		query.Tail = value
	}
	threadID, err := s.resolveThreadReference(r.Context(), scope.NativeIDs[0])
	if err != nil {
		writeThreadReferenceError(w, err)
		return
	}
	messages, err := collectThreadMessages(r.Context(), catalog, threadID, query)
	if err != nil {
		writeTranscriptBackendError(w, err)
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
			Preview: truncateUTF8Bytes(strings.TrimSpace(message.preview), pinAnchorPreviewBytes)})
	}
	writeCatalogJSON(w, http.StatusOK, map[string]any{
		"hostedThreadId": record.ID, "nativeThreadId": scope.NativeIDs[0], "order": "newest_first", "candidates": out,
	})
}

// ---- monitor ------------------------------------------------------------

type threadPinMonitorThread struct {
	HostedThreadID string `json:"hostedThreadId"`
	Pins           int    `json:"pins"`
	NewestAt       string `json:"newestAt"`
	ListCalls      int    `json:"listCalls"`
	ContinuedCalls int    `json:"continuedListCalls"`
	MaxListDepth   int    `json:"maxListDepth"`
}

type threadPinMonitorBroken struct {
	PinID          string `json:"pinId"`
	HostedThreadID string `json:"hostedThreadId"`
	NativeThreadID string `json:"nativeThreadId"`
	ItemID         string `json:"itemId"`
	Reason         string `json:"reason"`
}

type threadPinMonitorResponse struct {
	GeneratedAt  string `json:"generatedAt"`
	Note         string `json:"note"`
	Accumulation struct {
		TotalPins      int                      `json:"totalPins"`
		Threads        int                      `json:"threads"`
		CreatedLast24h int                      `json:"createdLast24h"`
		CreatedLast7d  int                      `json:"createdLast7d"`
		ByThread       []threadPinMonitorThread `json:"byThread"`
	} `json:"accumulation"`
	Retrieval struct {
		ListCalls          int     `json:"listCalls"`
		ContinuedListCalls int     `json:"continuedListCalls"`
		MaxListDepth       int     `json:"maxListDepth"`
		ContextReads       int     `json:"contextReads"`
		BrokenContextReads int     `json:"brokenContextReads"`
		PinsEverRead       int     `json:"pinsEverRead"`
		PinsNeverRead      int     `json:"pinsNeverRead"`
		MeanReadRank       float64 `json:"meanReadRank"`
		MaxReadRank        int     `json:"maxReadRank"`
	} `json:"retrieval"`
	Anchors struct {
		Checked   int                      `json:"checked"`
		Unchecked int                      `json:"unchecked"`
		Broken    []threadPinMonitorBroken `json:"broken"`
	} `json:"anchors"`
}

// handleThreadPinMonitor is observational only. It reads pins and the read
// counters and probes each anchor; it never writes, repairs, or deletes.
func (s *server) handleThreadPinMonitor(w http.ResponseWriter, r *http.Request) {
	catalog, ok := s.pinPreconditions(w)
	if !ok {
		return
	}
	var response threadPinMonitorResponse
	now := time.Now().UTC()
	response.GeneratedAt = now.Format(time.RFC3339Nano)
	response.Note = "Observational only: this report never modifies, prunes, or validates pins. Read rank 1 is the newest pin in its thread."
	response.Anchors.Broken = []threadPinMonitorBroken{}
	response.Accumulation.ByThread = []threadPinMonitorThread{}
	db := s.threadPins.db
	if err := func() error {
		dayAgo, weekAgo := now.Add(-24*time.Hour).Format(time.RFC3339Nano), now.Add(-7*24*time.Hour).Format(time.RFC3339Nano)
		if err := db.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT hosted_thread_id),
  COALESCE(SUM(created_at >= ?), 0), COALESCE(SUM(created_at >= ?), 0) FROM thread_pins`, dayAgo, weekAgo).Scan(
			&response.Accumulation.TotalPins, &response.Accumulation.Threads,
			&response.Accumulation.CreatedLast24h, &response.Accumulation.CreatedLast7d); err != nil {
			return err
		}
		rows, err := db.Query(`SELECT p.hosted_thread_id, COUNT(*), MAX(p.created_at),
  COALESCE(l.calls, 0), COALESCE(l.continued_calls, 0), COALESCE(l.max_depth, 0)
FROM thread_pins p LEFT JOIN thread_pin_list_reads l ON l.hosted_thread_id = p.hosted_thread_id
GROUP BY p.hosted_thread_id ORDER BY COUNT(*) DESC, MAX(p.seq) DESC LIMIT 50`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var thread threadPinMonitorThread
			if err := rows.Scan(&thread.HostedThreadID, &thread.Pins, &thread.NewestAt, &thread.ListCalls,
				&thread.ContinuedCalls, &thread.MaxListDepth); err != nil {
				rows.Close()
				return err
			}
			response.Accumulation.ByThread = append(response.Accumulation.ByThread, thread)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := db.QueryRow(`SELECT COALESCE(SUM(calls), 0), COALESCE(SUM(continued_calls), 0), COALESCE(MAX(max_depth), 0)
FROM thread_pin_list_reads`).Scan(&response.Retrieval.ListCalls, &response.Retrieval.ContinuedListCalls,
			&response.Retrieval.MaxListDepth); err != nil {
			return err
		}
		var rankTotal int
		if err := db.QueryRow(`SELECT COALESCE(SUM(reads), 0), COALESCE(SUM(broken_reads), 0), COALESCE(SUM(rank_total), 0),
  COALESCE(MAX(max_rank), 0), (SELECT COUNT(*) FROM thread_pins WHERE pin_id IN (SELECT pin_id FROM thread_pin_context_reads))
FROM thread_pin_context_reads`).Scan(&response.Retrieval.ContextReads, &response.Retrieval.BrokenContextReads,
			&rankTotal, &response.Retrieval.MaxReadRank, &response.Retrieval.PinsEverRead); err != nil {
			return err
		}
		response.Retrieval.PinsNeverRead = response.Accumulation.TotalPins - response.Retrieval.PinsEverRead
		if response.Retrieval.ContextReads > 0 {
			response.Retrieval.MeanReadRank = float64(rankTotal) / float64(response.Retrieval.ContextReads)
		}
		return nil
	}(); err != nil {
		httpapi.WriteError(w, http.StatusInternalServerError, "pin_monitor_error", "Pin monitor read failed")
		return
	}

	rows, err := db.Query(`SELECT `+threadPinColumns+` FROM thread_pins ORDER BY seq DESC LIMIT ?`, maxPinMonitorAnchorCheck)
	if err != nil {
		httpapi.WriteError(w, http.StatusInternalServerError, "pin_monitor_error", "Pin monitor read failed")
		return
	}
	var pins []threadPinRecord
	for rows.Next() {
		pin, err := scanThreadPin(rows)
		if err != nil {
			rows.Close()
			httpapi.WriteError(w, http.StatusInternalServerError, "pin_monitor_error", "Pin monitor read failed")
			return
		}
		pins = append(pins, pin)
	}
	rows.Close()
	for _, pin := range pins {
		reason := ""
		threadID, err := s.resolveThreadReference(r.Context(), pin.NativeThreadID)
		if err == nil {
			_, err = catalog.LocatePinAnchor(r.Context(), threadID, pin.AnchorItemID)
		}
		switch {
		case err == nil:
		case errors.Is(err, history.ErrPinAnchorNotFound):
			reason = "anchor_missing"
		case errors.Is(err, errCatalogThreadNotFound):
			reason = "thread_not_indexed"
		default:
			reason = "check_failed"
		}
		response.Anchors.Checked++
		if reason != "" {
			response.Anchors.Broken = append(response.Anchors.Broken, threadPinMonitorBroken{
				PinID: pin.PinID, HostedThreadID: pin.HostedThreadID, NativeThreadID: pin.NativeThreadID,
				ItemID: pin.AnchorItemID, Reason: reason,
			})
		}
	}
	response.Anchors.Unchecked = response.Accumulation.TotalPins - response.Anchors.Checked
	writeCatalogJSON(w, http.StatusOK, response)
}

var _ threadPinCatalog = (*threadCatalogService)(nil)
