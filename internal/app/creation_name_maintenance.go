package app

import "net/http"

func (s *server) handleCreationNameRepair(w http.ResponseWriter, r *http.Request) {
	s.creationNames().HandleRepair(w, r)
}
