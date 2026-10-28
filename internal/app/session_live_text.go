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
	applied()
	outboxChanged(...prompts.OutboxEntry)
	currentTexts(context.Context) []string
	requestPane(*http.Request) (sessionPane, error)
	rowsPane(conversationView) sessionPane
	conversationItems(context.Context, conversationView, string, int) ([]liveProjectionItem, *liveProjectionPage, string, error)
	writeBackendError(http.ResponseWriter, error)
}

// sessionPane exposes one ordered pane's bytes without its view-owned state.
type sessionPane interface {
	start()
	stop()
	doneChannel() <-chan struct{}
	readyChannel() <-chan struct{}
	take() [][]byte
}

// publishLiveOutbox keeps the optional view notification behind its session owner.
func (sess *session) publishLiveOutbox(entries ...prompts.OutboxEntry) {
	if sess.liveText != nil {
		sess.liveText.outboxChanged(entries...)
	}
}

func (sess *session) notifySavedTranscript() {
	if sess.liveText != nil {
		sess.liveText.applied()
	}
}
