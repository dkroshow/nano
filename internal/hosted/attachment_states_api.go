package hosted

import (
	"net/http"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/httpapi"
)

type attachmentStateView struct {
	HostedThreadID string `json:"hostedThreadId"`
	Provider       string `json:"provider"`
	NativeThreadID string `json:"nativeThreadId"`
	State          string `json:"state"`
	AttachedAt     string `json:"attachedAt"`
}

// HandleAttachmentStates lists every hosted thread Nano would bring back:
// attached or resumable, never finished. It is the cleanup agent's list to
// choose from; closing one (POST .../close with actorThreadRef) finishes it.
// ?state=attached or ?state=resumable narrows the list.
func (s *Controller) HandleAttachmentStates(w http.ResponseWriter, r *http.Request) {
	if s.Attachments == nil {
		httpapi.WriteHTTPError(w, "attachment store unavailable", http.StatusServiceUnavailable)
		return
	}
	state := r.URL.Query().Get("state")
	if state != "" && state != hostedstore.AttachmentAttached && state != hostedstore.AttachmentResumable {
		httpapi.WriteHTTPError(w, "state must be attached or resumable", http.StatusBadRequest)
		return
	}
	items := []attachmentStateView{}
	for _, rec := range s.Attachments.Records() {
		if state == "" || rec.State == state {
			items = append(items, attachmentStateView{HostedThreadID: rec.HostedThreadID, Provider: rec.Provider, NativeThreadID: rec.NativeThreadID, State: rec.State, AttachedAt: rec.AttachedAt})
		}
	}
	httpapi.WriteJSON(w, map[string]any{"items": items})
}
