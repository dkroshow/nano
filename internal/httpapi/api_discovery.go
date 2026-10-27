package httpapi

import "net/http"

// DiscoveryHandler serves an embedded API document with its discovery link.
func DiscoveryHandler(document []byte, link string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache")
		if link != "" {
			w.Header().Set("Link", link)
		}
		_, _ = w.Write(document)
	}
}
