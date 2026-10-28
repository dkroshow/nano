package app

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"time"

	"github.com/dkroshow/nano/internal/diagnostics"
)

// browserDuplicateIncident is the durable, content-free observation. Raw DOM
// keys and semantic message IDs are hashed before this record is written.
type browserDuplicateIncident = diagnostics.BrowserDuplicateIncident

type browserDuplicateIncidentFilter = diagnostics.BrowserDuplicateIncidentFilter

func contentFreeIdentityFingerprint(namespace string, values []string) string {
	seen := make(map[string]bool, len(values))
	canonical := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			canonical = append(canonical, value)
		}
	}
	if len(canonical) == 0 {
		return ""
	}
	sort.Strings(canonical)
	sum := sha256.Sum256([]byte(namespace + "\x00" + strings.Join(canonical, "\x00")))
	return hex.EncodeToString(sum[:])
}

func duplicateIdentitySets(rows []transcriptValidationRow) (rawKeys, semanticIDs []string) {
	keyCounts := map[string]int{}
	semanticCounts := map[string]int{}
	for _, row := range rows {
		keyCounts[row.Key]++
		if row.SemanticID != "" {
			semanticCounts[row.SemanticID]++
		}
	}
	for key, count := range keyCounts {
		if count > 1 {
			rawKeys = append(rawKeys, key)
		}
	}
	for id, count := range semanticCounts {
		if count > 1 {
			semanticIDs = append(semanticIDs, id)
		}
	}
	sort.Strings(rawKeys)
	sort.Strings(semanticIDs)
	return rawKeys, semanticIDs
}

func deriveBrowserDuplicateIncident(snapshot transcriptValidationSnapshot, receivedAt time.Time) (browserDuplicateIncident, bool) {
	rawKeys, semanticIDs := duplicateIdentitySets(snapshot.MountedRows)
	if len(rawKeys) == 0 && len(semanticIDs) == 0 {
		return browserDuplicateIncident{}, false
	}
	threadIdentity := snapshot.HostedThreadID
	if threadIdentity == "" {
		threadIdentity = snapshot.Provider + ":" + snapshot.ThreadID
	}
	if strings.Trim(threadIdentity, ":") == "" || snapshot.PaneID == "" {
		return browserDuplicateIncident{}, false
	}
	rawFingerprint := contentFreeIdentityFingerprint("browser-render-key-v1", rawKeys)
	semanticFingerprint := contentFreeIdentityFingerprint("browser-semantic-id-v1", semanticIDs)
	incidentFingerprint := contentFreeIdentityFingerprint("browser-duplicate-incident-v1", []string{
		"thread=" + threadIdentity, "raw=" + rawFingerprint, "semantic=" + semanticFingerprint,
	})
	observedAt := receivedAt.UTC()
	if checked, err := time.Parse(time.RFC3339Nano, snapshot.CheckedAt); err == nil {
		observedAt = checked.UTC()
	}
	stamp := observedAt.Format(time.RFC3339Nano)
	return browserDuplicateIncident{
		IncidentFingerprint: incidentFingerprint, Provider: snapshot.Provider,
		ThreadID: snapshot.ThreadID, HostedThreadID: snapshot.HostedThreadID,
		RawRenderKeyFingerprint: rawFingerprint, SemanticIDFingerprint: semanticFingerprint,
		RawRenderKeyDuplicateCount: len(rawKeys), SemanticIDDuplicateCount: len(semanticIDs),
		FirstBuild: snapshot.Build, LastBuild: snapshot.Build,
		FirstPaneID: snapshot.PaneID, LastPaneID: snapshot.PaneID,
		FirstObservedAt: stamp, LastObservedAt: stamp, ObservationCount: 1,
	}, true
}
