// Package api embeds the agent-facing discovery documents so the running binary
// is the source an invoking agent can trust.
package api

import _ "embed"

// CapabilitiesJSON describes the hosted-thread capabilities.
//
//go:embed capabilities.json
var CapabilitiesJSON []byte

// OpenAPIJSON describes the provider-neutral hosted-thread boundary,
// not Nano's browser-internal API.
//
//go:embed openapi.json
var OpenAPIJSON []byte
