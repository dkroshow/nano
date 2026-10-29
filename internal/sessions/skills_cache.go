package sessions

import (
	"net/http"

	"github.com/dkroshow/nano/internal/httpapi"
)

// HandleSkills serves a Codex session's skills list, read when the composer's
// Skills panel opens. It never joins provider fan-out: the shared cache
// answers at once and refreshes one answer per cwd in the background.
func (sess *View) HandleSkills(w http.ResponseWriter, _ *http.Request) {
	if sess.session.provider != "codex" {
		httpapi.WriteHTTPError(w, "the skills list is codex-only", http.StatusNotImplemented)
		return
	}
	sess.session.stateOwner.stateMu.Lock()
	cwd := sess.session.stateOwner.cwd
	sess.session.stateOwner.stateMu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Write(sess.skills.Current(cwd))
}
