package app

import (
	"github.com/dkroshow/nano/internal/httpapi"
	"net/http"

	"github.com/dkroshow/nano/api"
)

func apiDiscoveryRoutes() []applicationRoute {
	return []applicationRoute{
		{http.MethodGet, "/api/v1", "discovery.read", httpapi.DiscoveryHandler(api.CapabilitiesJSON, `</api/v1/openapi.json>; rel="service-desc"`)},
		{http.MethodGet, "/api/v1/capabilities", "capabilities.read", httpapi.DiscoveryHandler(api.CapabilitiesJSON, `</api/v1/openapi.json>; rel="service-desc"`)},
		{http.MethodGet, "/api/v1/openapi.json", "openapi.read", httpapi.DiscoveryHandler(api.OpenAPIJSON, `</api/v1/capabilities>; rel="service-meta"`)},
	}
}
