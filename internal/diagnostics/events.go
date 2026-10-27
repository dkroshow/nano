package diagnostics

import (
	"regexp"
	"strings"
	"sync"
	"time"
)

const DefaultCapacity = 10_000

var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9._:/#-]*$`)

// Event is deliberately content-free. It records identities, cursors, and
// state transitions needed to explain a stale pane, never user content.
type Event struct {
	Trigger                   string  `json:"trigger,omitempty"`
	View                      string  `json:"view,omitempty"`
	FirstRowsMS               *int    `json:"firstRowsMs,omitempty"`
	NewestRowsMS              *int    `json:"newestRowsMs,omitempty"`
	HistoryMS                 *int    `json:"historyMs,omitempty"`
	HistoryBytes              *int    `json:"historyBytes,omitempty"`
	EventID                   string  `json:"eventId"`
	ClientAt                  string  `json:"clientAt,omitempty"`
	ReceivedAt                string  `json:"receivedAt,omitempty"`
	ClientSeq                 int64   `json:"clientSeq,omitempty"`
	MonotonicMS               float64 `json:"monotonicMs,omitempty"`
	Kind                      string  `json:"kind"`
	TabID                     string  `json:"tabId,omitempty"`
	PageID                    string  `json:"pageId,omitempty"`
	Build                     string  `json:"build,omitempty"`
	PaneID                    string  `json:"paneId,omitempty"`
	Provider                  string  `json:"provider,omitempty"`
	ThreadID                  string  `json:"threadId,omitempty"`
	SessionID                 string  `json:"sessionId,omitempty"`
	PreviousSession           string  `json:"previousSessionId,omitempty"`
	HostedThreadID            string  `json:"hostedThreadId,omitempty"`
	ActionID                  string  `json:"actionId,omitempty"`
	InteractionID             string  `json:"interactionId,omitempty"`
	StreamID                  string  `json:"streamId,omitempty"`
	Epoch                     string  `json:"epoch,omitempty"`
	Revision                  int64   `json:"revision,omitempty"`
	IncomingRevision          int64   `json:"incomingRevision,omitempty"`
	ItemCount                 int     `json:"itemCount,omitempty"`
	PendingCount              int     `json:"pendingCount,omitempty"`
	ActiveRenderSource        string  `json:"activeRenderSource,omitempty"`
	VisibleItemCount          *int    `json:"visibleItemCount,omitempty"`
	MatchingDOMActionRowCount *int    `json:"matchingDOMActionRowCount,omitempty"`
	ExpectedRowCount          *int    `json:"expectedRowCount,omitempty"`
	MountedRowCount           *int    `json:"mountedRowCount,omitempty"`
	MissingRowCount           *int    `json:"missingRowCount,omitempty"`
	ExtraRowCount             *int    `json:"extraRowCount,omitempty"`
	DuplicateRowCount         *int    `json:"duplicateRowCount,omitempty"`
	SemanticDuplicateRowCount *int    `json:"semanticDuplicateRowCount,omitempty"`
	BlankRowCount             *int    `json:"blankRowCount,omitempty"`
	TypeMismatchCount         *int    `json:"typeMismatchCount,omitempty"`
	OrderMismatchCount        *int    `json:"orderMismatchCount,omitempty"`
	HTTPStatus                int     `json:"httpStatus,omitempty"`
	ArrivalRateCPS            int     `json:"arrivalRateCps,omitempty"`
	BatchIntervalMS           int     `json:"batchIntervalMs,omitempty"`
	BatchSize                 int     `json:"batchSize,omitempty"`
	BatchCount                int     `json:"batchCount,omitempty"`
	MaxBacklog                int     `json:"maxBacklog,omitempty"`
	PublishCount              int     `json:"publishCount,omitempty"`
	DurationMS                int     `json:"durationMs,omitempty"`
	LayoutSequence            int     `json:"layoutSequence,omitempty"`
	RowIndex                  *int    `json:"rowIndex,omitempty"`
	PreviousRowIndex          *int    `json:"previousRowIndex,omitempty"`
	TopPX                     *int    `json:"topPx,omitempty"`
	PreviousTopPX             *int    `json:"previousTopPx,omitempty"`
	MovementPX                int     `json:"movementPx,omitempty"`
	MovementCount             int     `json:"movementCount,omitempty"`
	MaxMovementPX             int     `json:"maxMovementPx,omitempty"`
	ReorderCount              int     `json:"reorderCount,omitempty"`
	AddedCount                int     `json:"addedCount,omitempty"`
	RemovedCount              int     `json:"removedCount,omitempty"`
	WorkingCount              int     `json:"workingCount,omitempty"`
	StarredCount              int     `json:"starredCount,omitempty"`
	DetachedCount             int     `json:"detachedCount,omitempty"`
	ScrollTop                 int     `json:"scrollTop,omitempty"`
	ScrollHeight              int     `json:"scrollHeight,omitempty"`
	ViewportHeight            int     `json:"viewportHeight,omitempty"`
	Visible                   *bool   `json:"visible,omitempty"`
	Connected                 *bool   `json:"connected,omitempty"`
	TurnActive                *bool   `json:"turnActive,omitempty"`
	// Warm marks a thread open whose pane first showed the warm stream's rows.
	Warm    *bool  `json:"warm,omitempty"`
	Status  string `json:"status,omitempty"`
	Outcome string `json:"outcome,omitempty"`
	Op      string `json:"op,omitempty"`
	Code    string `json:"code,omitempty"`
}

type Store struct {
	mu       sync.Mutex
	bootID   string
	capacity int
	events   []Event
	seen     map[string]struct{}
}

func NewStore(capacity int, bootID string) *Store {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	return &Store{bootID: bootID, capacity: capacity, seen: make(map[string]struct{})}
}

func (store *Store) BootID() string {
	if store == nil {
		return ""
	}
	return store.bootID
}

func (store *Store) Add(events []Event) int {
	if store == nil {
		return 0
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	accepted := 0
	for _, event := range events {
		if !NormalizeEvent(&event) {
			continue
		}
		if _, exists := store.seen[event.EventID]; exists {
			continue
		}
		event.ReceivedAt = time.Now().UTC().Format(time.RFC3339Nano)
		store.events = append(store.events, event)
		store.seen[event.EventID] = struct{}{}
		accepted++
	}
	if overflow := len(store.events) - store.capacity; overflow > 0 {
		for _, event := range store.events[:overflow] {
			delete(store.seen, event.EventID)
		}
		copy(store.events, store.events[overflow:])
		store.events = store.events[:store.capacity]
	}
	return accepted
}

func (store *Store) Recent(limit int, threadID, tabID, kind string) []Event {
	if store == nil {
		return nil
	}
	if limit <= 0 {
		limit = 200
	}
	if limit > 2_000 {
		limit = 2_000
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	result := make([]Event, 0, limit)
	for index := len(store.events) - 1; index >= 0 && len(result) < limit; index-- {
		event := store.events[index]
		if threadID != "" && event.ThreadID != threadID || tabID != "" && event.TabID != tabID || kind != "" && event.Kind != kind {
			continue
		}
		result = append(result, event)
	}
	for left, right := 0, len(result)-1; left < right; left, right = left+1, right-1 {
		result[left], result[right] = result[right], result[left]
	}
	return result
}

func NormalizeEvent(event *Event) bool {
	if event == nil {
		return false
	}
	event.EventID = Token(event.EventID, 160)
	event.Kind = Token(event.Kind, 80)
	if event.EventID == "" || event.Kind == "" {
		return false
	}
	event.ClientAt = Time(event.ClientAt)
	event.TabID = Token(event.TabID, 100)
	event.PageID = Token(event.PageID, 100)
	event.Build = Token(event.Build, 100)
	event.PaneID = Token(event.PaneID, 100)
	event.Provider = Token(event.Provider, 32)
	event.ThreadID = Token(event.ThreadID, 160)
	event.SessionID = Token(event.SessionID, 100)
	event.PreviousSession = Token(event.PreviousSession, 100)
	event.HostedThreadID = Token(event.HostedThreadID, 160)
	event.ActionID = Token(event.ActionID, 160)
	event.InteractionID = Token(event.InteractionID, 160)
	event.StreamID = Token(event.StreamID, 100)
	event.Epoch = Token(event.Epoch, 160)
	event.Status = Token(event.Status, 80)
	event.Outcome = Token(event.Outcome, 80)
	switch event.View {
	case "enhanced", "dev":
	default:
		event.View = ""
	}
	switch event.Trigger {
	case "sidebar", "tab", "load":
	default:
		event.Trigger = ""
	}
	event.Op = Token(event.Op, 80)
	event.Code = Token(event.Code, 100)
	event.ActiveRenderSource = Token(event.ActiveRenderSource, 32)
	for _, count := range []*int{
		event.FirstRowsMS, event.NewestRowsMS, event.HistoryMS, event.HistoryBytes,
		&event.ItemCount, &event.PendingCount, event.VisibleItemCount,
		event.MatchingDOMActionRowCount, event.ExpectedRowCount, event.MountedRowCount,
		event.MissingRowCount, event.ExtraRowCount, event.DuplicateRowCount,
		event.SemanticDuplicateRowCount, event.BlankRowCount, event.TypeMismatchCount,
		event.OrderMismatchCount, &event.ArrivalRateCPS, &event.BatchIntervalMS,
		&event.BatchSize, &event.BatchCount, &event.MaxBacklog, &event.PublishCount,
		&event.DurationMS, event.RowIndex, event.PreviousRowIndex, event.TopPX,
		event.PreviousTopPX, &event.LayoutSequence, &event.MovementPX,
		&event.MovementCount, &event.MaxMovementPX, &event.ReorderCount,
		&event.AddedCount, &event.RemovedCount, &event.WorkingCount,
		&event.StarredCount, &event.DetachedCount, &event.ScrollTop,
		&event.ScrollHeight, &event.ViewportHeight,
	} {
		if count != nil && *count < 0 {
			*count = 0
		}
	}
	return true
}

func Token(value string, maxLength int) string {
	value = strings.TrimSpace(value)
	if len(value) > maxLength {
		value = value[:maxLength]
	}
	if !tokenPattern.MatchString(value) {
		return ""
	}
	return value
}

func Time(value string) string {
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	if err != nil {
		return ""
	}
	return parsed.UTC().Format(time.RFC3339Nano)
}
