package sessions

import (
	"context"
)

// ReconcileSavedTranscript confirms saved items and settles their waiting prompts.
func ReconcileSavedTranscript(ctx context.Context, sess *Session) {
	if sess == nil {
		return
	}
	sess.ReconcileDurableTurn(ctx)
	sess.SettleWaitingPrompts(ctx)
}
