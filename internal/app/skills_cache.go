package app

import (
	"net/http"

	"github.com/dkroshow/nano/internal/httpapi"
)

// handleSkills serves a Codex session's skills list, read when the composer's
// skills panel opens. It never joins provider fan-out: the shared cache
// answers at once and refreshes one answer per cwd in the background.
func (sess *session) handleSkills(w http.ResponseWriter, _ *http.Request) {
	if sess.provider != "codex" {
		httpapi.WriteHTTPError(w, "the skills list is codex-only", http.StatusNotImplemented)
		return
	}
	sess.stateMu.Lock()
	cwd := sess.cwd
	sess.stateMu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Write(sess.skills.Current(cwd))
}
