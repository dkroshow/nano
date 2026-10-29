package hosted

import (
	"encoding/json"
	"net/http"

	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/httpapi"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/threadindex"
)

// handleSessionName renames a live session before or after its native thread
// is indexed. A fresh Claude session has no native id until its first turn, so
// its validated name remains pending until applyPendingName can persist it.
// HandleSessionName renames a live or provisional attachment through the session owner.
func (s *AttachmentCoordinator) HandleSessionName(w http.ResponseWriter, r *http.Request, sess *sessions.Session, namer threadindex.Namer) {
	w.Header().Set("Cache-Control", "private, no-store")
	if namer == nil {
		httpapi.WriteError(w, http.StatusServiceUnavailable, "catalog_unavailable", "Thread catalog unavailable")
		return
	}
	var payload struct {
		Name string `json:"name"`
	}
	body := http.MaxBytesReader(w, r.Body, threadindex.MaxThreadNameRequestBytes)
	if err := json.NewDecoder(body).Decode(&payload); err != nil {
		httpapi.WriteError(w, http.StatusBadRequest, "invalid_name", `Request body must be {"name": "..."}`)
		return
	}
	name, err := history.ValidateThreadName(payload.Name)
	if err != nil {
		threadindex.WriteThreadNameError(w, threadindex.NameInvalidError{Reason: err.Error()})
		return
	}

	// A native id, once present, is immutable for the hosted session. Holding
	// the lock while merely recording a pre-bind name lets applyPendingName see
	// exactly this value when the provider announces its identity.
	provider, nativeID, staged := sess.View().StageNameBeforeBinding(name)
	if staged {
		s.NotifyRuntimeRefresh()
		httpapi.WriteJSON(w, sess.View().Summary())
		return
	}

	// A provisional Claude identity is the shim handle standing in for a native
	// id that has not been announced yet (attachmentRecord.provisional). A
	// catalog write here would key the name to a handle captureInit is about to
	// retire, and clearing pendingName would leave applyPendingName nothing to
	// apply — the name would vanish with the first prompt. Stage it like the
	// pre-bind branch instead, and persist it onto the provisional record so a
	// Nano restart in this window keeps the rename. captureInit updates threadID
	// under stateMu before it consumes pendingName, so an unchanged threadID
	// proves the staged value will still be picked up.
	if rec, ok := s.attachmentFor(provider, nativeID); ok && rec.Provisional() {
		var staged bool
		nativeID, staged = sess.View().StageProvisionalName(nativeID, name)
		if staged {
			s.setAttachmentPendingName(provider, nativeID, name)
			s.NotifyRuntimeRefresh()
			httpapi.WriteJSON(w, sess.View().Summary())
			return
		}
		// The real id landed while we probed; assign under it normally.
	}

	if err := namer.AssignThreadName(provider, nativeID, name); err != nil {
		threadindex.WriteThreadNameError(w, err)
		return
	}
	sess.View().SetDisplayName(name)
	s.NotifyRuntimeRefresh()
	httpapi.WriteJSON(w, sess.View().Summary())
}
