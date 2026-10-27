package main

import (
	"context"
	"regexp"
	"strings"
)

// Human-facing identity: Nano display name first,
// native thread UUID when an id is needed, hosted id (`nht_…`) never — except
// as a marked fallback when Nano cannot resolve it. Hosted ids stay valid as
// input (API paths, invokerThreadRef, ?thread=). These helpers render at the
// boundary; stored records keep hosted ids.

var hostedIDPattern = regexp.MustCompile(`nht_[A-Za-z0-9_-]+`)

func unresolvedHostedMarker(hostedID string) string {
	return hostedID + " (unresolved hosted id)"
}

// formatAgentIdentity renders `name (provider, native uuid)`, degrading when
// a half is missing and falling back to the marker when nothing is known.
func formatAgentIdentity(displayName, provider, nativeThreadID, hostedID string) string {
	name := strings.TrimSpace(displayName)
	var parts []string
	if provider != "" {
		parts = append(parts, provider)
	}
	if nativeThreadID != "" {
		parts = append(parts, nativeThreadID)
	}
	detail := strings.Join(parts, ", ")
	switch {
	case name != "" && detail != "":
		return name + " (" + detail + ")"
	case name != "":
		return name
	case detail != "":
		return "(" + detail + ")"
	default:
		return unresolvedHostedMarker(hostedID)
	}
}

// agentDisplay renders one hosted thread for people. It reads the canonical
// hosted record and the catalog only — never live session summaries — so it
// is safe to call while a session's sendMu is held (the notification
// dispatcher does exactly that; session.summary() takes sendMu).
func (s *server) agentDisplay(ctx context.Context, hostedID string) string {
	if s == nil || hostedID == "" {
		return unresolvedHostedMarker(hostedID)
	}
	rec, ok, err := s.hostedCanonical(hostedID)
	if err != nil || !ok {
		return unresolvedHostedMarker(hostedID)
	}
	name := s.displayNameForHosted(ctx, rec.Provider, rec.ID, rec.NativeThreadID)
	return formatAgentIdentity(name, rec.Provider, rec.NativeThreadID, hostedID)
}
