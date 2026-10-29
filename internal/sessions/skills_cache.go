package sessions

import (
	"net/http"

	"github.com/dkroshow/nano/internal/httpapi"
)

// HandleSkills serves a Codex session's skills list, read when the composer's
// Skills panel opens. It never joins provider fan-out: the shared cache
// answers at once and refreshes one answer per cwd in the background.
func (attachmentState *State) HandleSkills(w http.ResponseWriter, _ *http.Request) {
	if attachmentState.identity.provider != "codex" {
		httpapi.WriteHTTPError(w, "the skills list is codex-only", http.StatusNotImplemented)
		return
	}
	attachmentState.stateMu.Lock()
	cwd := attachmentState.cwd
	attachmentState.stateMu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Write(attachmentState.view.skillsFor(cwd))
}
