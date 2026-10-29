package store

import (
	"strings"
)

// NativeThreadIDProvisional reports an unresolved Claude shim handle rather than a provider identity.
func NativeThreadIDProvisional(provider, nativeID string) bool {
	return provider == "claude" && strings.HasPrefix(nativeID, "cs_")
}

// StatusExited is the terminal hosted attachment status retained in execution records.
const StatusExited = "exited"
