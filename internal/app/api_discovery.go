package app

import (
	"net/http"

	"github.com/dkroshow/nano/api"
)

func handleAPICapabilities(w http.ResponseWriter, r *http.Request) {
	serveAPIDiscoveryDocument(w, r, api.CapabilitiesJSON, `</api/v1/openapi.json>; rel="service-desc"`)
}

func handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	serveAPIDiscoveryDocument(w, r, api.OpenAPIJSON, `</api/v1/capabilities>; rel="service-meta"`)
}

func apiDiscoveryRoutes() []applicationRoute {
	return []applicationRoute{
		{http.MethodGet, "/api/v1", "discovery.read", handleAPICapabilities},
		{http.MethodGet, "/api/v1/capabilities", "capabilities.read", handleAPICapabilities},
		{http.MethodGet, "/api/v1/openapi.json", "openapi.read", handleOpenAPI},
	}
}

func registerAPIDiscoveryRoutes(mux *http.ServeMux) {
	mountExactRoutes(mux, apiDiscoveryRoutes())
}

func serveAPIDiscoveryDocument(w http.ResponseWriter, r *http.Request, document []byte, link string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	if link != "" {
		w.Header().Set("Link", link)
	}
	_, _ = w.Write(document)
}
