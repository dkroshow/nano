package sessions

import (
	"net/http"

	"github.com/dkroshow/nano/internal/httpapi"
)

// SessionOperation resolves the route attachment before dispatching its operation.
func (s *Registry) SessionOperation(operation func(http.ResponseWriter, *http.Request, *Session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("sessionId")
		sess := s.LookupSession(id)
		if sess == nil {
			httpapi.WriteHTTPError(w, "no such session", http.StatusNotFound)
			return
		}
		operation(w, r, sess)
	}
}
