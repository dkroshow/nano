package app

import "fmt"

// nanoIdentityContext renders the immutable identity block Nano injects into a
// hosted agent's trusted context at session start (Codex: appended to
// developerInstructions; Claude: --append-system-prompt; Grok: no seam, see
// docs). Only values stable for the thread's whole lifetime appear here — the
// provider-native id is unknown at start and can be replaced by a later
// --resume child, so agents are pointed at the hosted-thread endpoint for
// everything current.
func nanoIdentityContext(hostedThreadID, provider, apiOrigin string) string {
	return fmt.Sprintf(`Nano hosted identity (immutable, server-injected)
hostedThreadId: %s
provider: %s
apiOrigin: %s
This hostedThreadId is your durable Nano identity: retain it, and cite it as invokerThreadRef when creating hosted threads. Other ids rotate (nanoSessionId, per attachment) or can be replaced (nativeThreadId, on resume). Current details: GET %s/api/v1/hosted-threads/%s\nDurable mail: POST %s/api/v1/hosted-threads/<recipientRef>/messages and read your own inbox at GET %s/api/v1/hosted-threads/%s/inbox?actorThreadRef=%s. Inbox mail never creates a prompt turn. A normal inbox read marks mail seen; disposition every seen message with POST %s/api/v1/messages/<messageId>/status as acted, declined, or deferred.\nNaming: when you refer to yourself or any agent in text for people, use the Nano display name, or the native thread UUID when an id is needed. Do not show a hostedThreadId to people; resolve any reference with GET %s/api/v1/identity/resolve?ref=<name|uuid|nht_…>.`,
		hostedThreadID, provider, apiOrigin, apiOrigin, hostedThreadID, apiOrigin, apiOrigin, hostedThreadID, hostedThreadID, apiOrigin, apiOrigin)
}

// identityInjectable reports whether a resolved spawn request carries enough
// to mint the block: a bound hosted identity plus this server's origin. Fresh
// starts and Claude fallback resumes both provide it; a fallback therefore
// preserves identity even when no attachment argv survived.
func identityInjectable(req providerRequest) bool {
	return req.HostedThreadID != "" && req.apiOrigin != ""
}
