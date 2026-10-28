package app

import (
	"context"
	"net/http"

	"github.com/dkroshow/nano/internal/core/prompts"
)

// sessionLiveText is the ordered view service consumed by session HTTP adapters.
// The view owns saved/live reconciliation and its own locks.
type sessionLiveText interface {
	liveTextObserver
	// Applied notifies the view that saved transcript evidence changed.
	Applied()
	// OutboxChanged notifies the view that prompt delivery evidence changed.
	OutboxChanged(...prompts.OutboxEntry)
	// CurrentTexts reads the currently displayed live text.
	CurrentTexts(context.Context) []string
	// RequestPane creates a pane for the validated request parameters.
	RequestPane(*http.Request) (sessionPane, error)
	// RowsPane creates a saved-row pane for a conversation view.
	RowsPane(conversationView) sessionPane
	// ConversationItems reads a bounded page of conversation items.
	ConversationItems(context.Context, conversationView, string, int) ([]liveProjectionItem, *liveProjectionPage, string, error)
	// WriteBackendError writes a transcript backend failure to the HTTP response.
	WriteBackendError(http.ResponseWriter, error)
}

// sessionPane exposes one ordered pane's bytes without its view-owned state.
type sessionPane interface {
	// Start starts the pane’s ordered stream.
	Start()
	// Stop releases the pane’s subscription.
	Stop()
	// DoneChannel closes when the pane stream ends.
	DoneChannel() <-chan struct{}
	// ReadyChannel signals that pane bytes are available.
	ReadyChannel() <-chan struct{}
	// Take takes the pane’s pending encoded bytes.
	Take() [][]byte
}

// publishLiveOutbox keeps the optional view notification behind its session owner.
func (sess *session) publishLiveOutbox(entries ...prompts.OutboxEntry) {
	if sess.liveText != nil {
		sess.liveText.OutboxChanged(entries...)
	}
}

func (sess *session) notifySavedTranscript() {
	if sess.liveText != nil {
		sess.liveText.Applied()
	}
}
