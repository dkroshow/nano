package hosted

import (
	"log"
	"net/http"
	"strings"
)
import "github.com/dkroshow/nano/internal/httpapi"

// HandleLegacyCreate serializes legacy creation with hosted relationship mutations.
func (s *Controller) HandleLegacyCreate(w http.ResponseWriter, r *http.Request) {
	s.relationshipMu.Lock()
	defer s.relationshipMu.Unlock()
	s.LegacyCreate(w, r)
}

// HandleLegacyClose closes a pinned legacy session under the relationship lock.
func (s *Controller) HandleLegacyClose(w http.ResponseWriter, _ *http.Request, sess CloseSession) {
	s.relationshipMu.Lock()
	defer s.relationshipMu.Unlock()
	if err := s.closeExplicitSession(sess); err != nil {
		httpapi.WriteHTTPError(w, err.Error(), http.StatusBadGateway)
		return
	}
	log.Printf("closed %s session %s", sess.Provider(), sess.ID())
	httpapi.WriteJSON(w, map[string]bool{"closed": true})
}

// HandleAttachmentScoped handles DELETE /api/attachments/{nativeThreadId}:
// stop a detached Claude process without re-attaching or replaying it.
// Ambiguous provider identity is refused rather than broadening the target.
func (s *Controller) HandleAttachmentScoped(w http.ResponseWriter, r *http.Request) {
	s.relationshipMu.Lock()
	defer s.relationshipMu.Unlock()
	nativeID := r.PathValue("nativeThreadId")
	if nativeID == "" {
		httpapi.WriteHTTPError(w, "native thread id required", http.StatusBadRequest)
		return
	}
	forget := r.URL.Query().Get("forget")
	if forget != "" && (forget != "unconfirmed" || r.URL.Query().Get("provider") != "claude" || !r.URL.Query().Has("expectedShimSessionId") || !r.URL.Query().Has("expectedAttachedAt")) {
		httpapi.WriteHTTPError(w, "unconfirmed forget requires Claude and the captured attachment identity", http.StatusBadRequest)
		return
	}
	if s.Attachments != nil {
		provider := strings.TrimSpace(r.URL.Query().Get("provider"))
		var matches []attachmentRecord
		for _, record := range s.Attachments.records() {
			if record.NativeThreadID == nativeID && (provider == "" || record.Provider == provider) {
				matches = append(matches, record)
			}
		}
		if len(matches) > 1 {
			httpapi.WriteHTTPError(w, "provider required for ambiguous attachment", http.StatusConflict)
			return
		}
		if len(matches) == 1 {
			var err error
			if forget == "unconfirmed" {
				err = s.forgetUnconfirmedClaude(matches[0], r.URL.Query().Get("expectedShimSessionId"), r.URL.Query().Get("expectedAttachedAt"))
			} else {
				err = s.closeExplicitAttachment(matches[0])
			}
			if err != nil {
				httpapi.WriteHTTPError(w, err.Error(), http.StatusBadGateway)
				return
			}
		}
		// The dismissed row's detached overlay just disappeared; nudge open
		// Explorers the same way bind/unbind does.
		s.NotifyRuntime()
	}
	if forget == "unconfirmed" {
		httpapi.WriteJSON(w, map[string]any{"forgotten": true, "processState": "unknown"})
		return
	}
	httpapi.WriteJSON(w, map[string]bool{"forgotten": true})
}
