package app

import (
	"database/sql"
	"testing"
)

const maxPinTextBytes = 500

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

type threadPinListResponse struct {
	HostedThreadID string          `json:"hostedThreadId"`
	Order          string          `json:"order"`
	Count          int             `json:"count"`
	Pins           []threadPinView `json:"pins"`
	HasMore        bool            `json:"hasMore"`
	NextCursor     *string         `json:"nextCursor"`
}

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

func (f *pinFixture) pinDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", f.pinsPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func (f *pinFixture) appendPin(t *testing.T, p threadPinRecord) error {
	t.Helper()
	_, err := f.pinDB(t).Exec(`INSERT INTO thread_pins(pin_id,hosted_thread_id,provider,native_thread_id,anchor_item_id,anchor_turn_id,anchor_type,anchor_preview,text,created_at) VALUES (?,?,?,?,?,?,?,?,?,?)`, p.PinID, p.HostedThreadID, p.Provider, p.NativeThreadID, p.AnchorItemID, p.AnchorTurnID, p.AnchorType, p.AnchorPreview, p.Text, p.CreatedAt)
	return err
}
