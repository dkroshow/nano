package sessions

import (
	"github.com/dkroshow/nano/internal/core/prompts"
)

// PublishLiveOutbox keeps the optional view notification behind its session owner.
func (sess *Session) PublishLiveOutbox(entries ...prompts.OutboxEntry) {
	if sess.liveText != nil {
		sess.liveText.OutboxChanged(entries...)
	}
}

// NotifySavedTranscript notifies the live view that its saved transcript changed.
func (sess *Session) NotifySavedTranscript() {
	if sess.liveText != nil {
		sess.liveText.Applied()
	}
}
