package main

import (
	_ "embed"
	"net/http"
)

// The agent-facing discovery documents are embedded beside the web UI so the
// running binary is the source an invoking agent can trust. They describe the
// provider-neutral hosted-thread boundary, not Nano's browser-internal API.
//
//go:embed api/capabilities.json
var apiCapabilitiesJSON []byte

//go:embed api/openapi.json
var apiOpenAPIJSON []byte

func handleAPICapabilities(w http.ResponseWriter, r *http.Request) {
	serveAPIDiscoveryDocument(w, r, apiCapabilitiesJSON, `</api/v1/openapi.json>; rel="service-desc"`)
}

func handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	serveAPIDiscoveryDocument(w, r, apiOpenAPIJSON, `</api/v1/capabilities>; rel="service-meta"`)
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
