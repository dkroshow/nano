package sidebar

import (
	"net/http"

	"github.com/dkroshow/nano/internal/config"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/sessions"
)

// SessionsHandler serves live and detached session projections with launch defaults.
func (s *Controller) SessionsHandler(defaults config.SessionDefaults) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		// The position comes first: a change after it is either in this answer or
		// replayed over it, never missed.
		var position *RuntimePosition
		if s.runtimeEvents != nil {
			at := s.runtimeEvents.Position()
			position = &at
		}
		held := s.registry.SnapshotSessions()

		listed := s.ListedSessions(held)
		live := make(map[hostedstore.AttachmentKey]bool, len(listed))
		for _, sum := range listed {
			if sum.ThreadID != "" && sum.Status != sessions.StatusExited {
				live[hostedstore.AttachmentKey{Provider: sum.Provider, NativeThreadID: sum.ThreadID}] = true
			}
		}
		httpapi.WriteJSON(w, map[string]any{
			"sessions":      listed,
			"detached":      s.DetachedSessionViews(live),
			"runtimeStream": position,
			"defaults": map[string]string{
				"cwd": defaults.CWD, "model": defaults.Model,
				"sandbox": defaults.Sandbox, "approval": defaults.Approval,
				"allowedTools": defaults.AllowedTools, "claudeBin": defaults.ClaudeBin,
				"grokBin": defaults.GrokBin, "grokLeaderSocket": defaults.GrokLeaderSocket,
			},
		})

	}
}
