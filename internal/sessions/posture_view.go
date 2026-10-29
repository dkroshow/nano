package sessions

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
)

// posture_view.go — the one posture struct every surface serves (coordination
// plan, decision 4): /api/sessions summaries, /state, and the catalog's
// memory rows all project from sessionPosture instead of hand-assembling
// disjoint field sets. Values come from control.sqlite's immutable hosted
// event log and current-state projection; live transport and retired JSON are
// never a fallback for a hosted identity.

// overlayFold writes the folded decision for one field over dst, leaving dst
// alone when the log holds no decision for it.
func overlayFold(folded map[string]hostedstore.PostureEntry, field string, dst *string) {
	if e, ok := folded[field]; ok {
		*dst = e.Value
	}
}

// posture is the canonical hosted-thread display projection. Legacy JSON is a
// migration input only; unavailable metadata stays unknown rather than being
// silently reconstructed from the retired posture log or local defaults.
func (attachmentState *State) posture() hostedstore.Posture {
	if attachmentState.identity.metadata == nil {
		return hostedstore.Posture{}
	}
	return attachmentState.identity.metadata.Posture(attachmentState.identity.hostedThreadID)
}

// markMetadataDegraded reports that a newer observation did not enter the
// canonical log. The last committed record remains readable and is still the
// only value authority; this flag is an ingestion warning, not a second
// availability decision.
func (attachmentState *State) markMetadataDegraded() {
	attachmentState.stateMu.Lock()
	attachmentState.metadataDegraded = true
	attachmentState.stateMu.Unlock()
}

// recordPosture is the one live mutation adapter.  It writes immutable events
// to control.sqlite only; canonical posture store is a read-only migration and
// rollback parser and is never dual-written by production code.
func (attachmentState *State) recordPosture(entries []hostedstore.PostureEntry, freshness string) {
	if attachmentState.identity.metadata == nil {
		return
	}
	committed, failed := false, false
	for _, entry := range entries {
		disposition := metadataDispositionRequested
		if entry.Value == "" {
			disposition = metadataDispositionCleared
		}
		if entry.Source == postureSourceProvider {
			disposition = metadataDispositionConfirmed
		}
		appended, err := attachmentState.identity.metadata.AppendEvent(hostedstore.MetadataEvent{
			HostedThreadID: attachmentState.identity.hostedThreadID, Field: entry.Field, Value: entry.Value,
			Disposition: disposition, Source: entry.Source, Evidence: entry.Evidence,
			NativeThreadID: attachmentState.ThreadID(), NanoSessionID: attachmentState.identity.id,
			AttachmentGeneration: attachmentState.attachmentGeneration, Freshness: freshness,
		})
		if err != nil {
			attachmentState.markMetadataDegraded()
			failed = true
			log.Printf("record canonical posture %s/%s: %v", attachmentState.identity.hostedThreadID, entry.Field, err)
		} else if appended.Committed {
			committed = true
		}
	}
	// The process-local runtime stream is intentionally only a wake-up hint.
	// Every open tab re-fetches /state, whose hosted metadata view and revision
	// are authoritative even if a notification is coalesced or lost.
	if committed || failed {
		attachmentState.NotifyRuntime()
	}
}

// providerCausation is an immutable record of the precise operator request
// that an outbound provider call carried.  It is deliberately captured before
// the RPC, rather than discovered when a response arrives: a later operator
// choice must never be settled by an older response.
type providerCausation struct {
	EventID int64
	Value   string
}

type providerCausationSnapshot map[string]providerCausation

// snapshotProviderCausation captures only requested values that are exactly
// represented in the outbound request.  Fields absent from values have no
// causal relationship to the response.
func (attachmentState *State) snapshotProviderCausation(values map[string]string) providerCausationSnapshot {
	if attachmentState.identity.metadata == nil || len(values) == 0 {
		return nil
	}
	view, err := attachmentState.identity.metadata.View(attachmentState.identity.hostedThreadID)
	if err != nil {
		return nil
	}
	out := providerCausationSnapshot{}
	for field, sent := range values {
		current, ok := view.Fields[field]
		if !ok || current.DesiredEventID == nil || current.DesiredDisposition != metadataDispositionRequested {
			continue
		}
		var desired string
		if json.Unmarshal(current.DesiredValue, &desired) == nil && desired == sent {
			out[field] = providerCausation{EventID: *current.DesiredEventID, Value: desired}
		}
	}
	return out
}

type turnCausationEntry struct {
	ID       int64
	Snapshot providerCausationSnapshot
}

// beginTurnCausation is the pre-Call seam used by sendCodexTurn.  Its return
// ID lets the error path remove the exact request rather than leaving stale
// causation for a later provider notification.
func (attachmentState *State) beginTurnCausation(values map[string]string) int64 {
	snapshot := attachmentState.snapshotProviderCausation(values)
	if len(snapshot) == 0 {
		return 0
	}
	attachmentState.stateMu.Lock()
	defer attachmentState.stateMu.Unlock()
	attachmentState.turnCausationSeq++
	id := attachmentState.turnCausationSeq
	attachmentState.pendingTurnCausation = append(attachmentState.pendingTurnCausation, turnCausationEntry{ID: id, Snapshot: snapshot})
	return id
}

func (attachmentState *State) abandonTurnCausation(id int64) {
	if id == 0 {
		return
	}
	attachmentState.stateMu.Lock()
	defer attachmentState.stateMu.Unlock()
	for i, entry := range attachmentState.pendingTurnCausation {
		if entry.ID == id {
			attachmentState.pendingTurnCausation = append(attachmentState.pendingTurnCausation[:i], attachmentState.pendingTurnCausation[i+1:]...)
			return
		}
	}
}

// takeSettingsCausation either consumes the next outbound turn request or,
// for a provider retry, returns the previous entry for this same semantic
// payload.  A second queued request always wins over retry dedupe, even if its
// settings are byte-for-byte identical.
func (attachmentState *State) takeSettingsCausation(semanticKey string) (turnCausationEntry, bool) {
	attachmentState.stateMu.Lock()
	defer attachmentState.stateMu.Unlock()
	if len(attachmentState.pendingTurnCausation) > 0 {
		entry := attachmentState.pendingTurnCausation[0]
		attachmentState.pendingTurnCausation = attachmentState.pendingTurnCausation[1:]
		if attachmentState.lastSettingsEvidence == nil {
			attachmentState.lastSettingsEvidence = map[string]turnCausationEntry{}
		}
		attachmentState.lastSettingsEvidence[semanticKey] = entry
		return entry, true
	}
	entry, ok := attachmentState.lastSettingsEvidence[semanticKey]
	return entry, ok
}

func providerEvidenceKey(hosted string, generation int64, seam, field, actual, freshness string, causal *providerCausation, kind string) string {
	causalID := int64(0)
	if causal != nil {
		causalID = causal.EventID
	}
	// Values can contain path separators and whitespace.  Quoted formatting is
	// stable, unambiguous, and makes retry equivalence include every semantic
	// dimension of the evidence.
	return fmt.Sprintf("provider:%q:%d:%q:%q:%q:%q:%d:%s", hosted, generation, seam, field, actual, freshness, causalID, kind)
}

// recordProviderEvidence is the sole provider-evidence reconciliation seam.
// A matching causal response confirms one event.  A mismatch records two
// facts: the exact requested value was rejected, and the provider actually
// reports a different confirmed value.  Replayed/historical evidence is
// never causal, even if a request snapshot happens to be available.
func (attachmentState *State) recordProviderEvidence(seam string, values map[string]string, freshness string, snapshot providerCausationSnapshot) {
	if attachmentState.identity.metadata == nil || len(values) == 0 {
		return
	}
	attachmentState.stateMu.Lock()
	generation := attachmentState.attachmentGeneration
	nativeID := attachmentState.threadID
	attachmentState.stateMu.Unlock()
	if freshness != metadataFreshnessLive {
		snapshot = nil
	}
	committed, failed := false, false
	for field, actual := range values {
		if strings.TrimSpace(actual) == "" {
			continue
		}
		causal, causalOK := snapshot[field]
		appendOne := func(value, disposition, kind string, cause *providerCausation) {
			var causeID *int64
			if cause != nil {
				id := cause.EventID
				causeID = &id
			}
			appended, err := attachmentState.identity.metadata.AppendEvent(hostedstore.MetadataEvent{
				HostedThreadID: attachmentState.identity.hostedThreadID, Field: field, Value: value,
				Disposition: disposition, Source: postureSourceProvider, Evidence: seam,
				NativeThreadID: nativeID, NanoSessionID: attachmentState.identity.id,
				AttachmentGeneration: generation, Freshness: freshness,
				CausationEventID: causeID,
				SourceEventKey:   providerEvidenceKey(attachmentState.identity.hostedThreadID, generation, seam, field, actual, freshness, cause, kind),
			})
			if err != nil {
				attachmentState.markMetadataDegraded()
				failed = true
				log.Printf("record provider evidence %s/%s: %v", attachmentState.identity.hostedThreadID, field, err)
			} else if appended.Committed {
				committed = true
			}
		}
		if causalOK && causal.Value != actual {
			appendOne(causal.Value, metadataDispositionRejected, "rejected", &causal)
			appendOne(actual, metadataDispositionConfirmed, "actual", nil)
			continue
		}
		if causalOK {
			appendOne(actual, metadataDispositionConfirmed, "confirmed", &causal)
		} else {
			appendOne(actual, metadataDispositionConfirmed, "observed", nil)
		}
	}
	if committed || failed {
		attachmentState.NotifyRuntime()
	}
}

// recordProviderRejection settles only the exact outbound requests a provider
// explicitly rejected. Timeouts and transport loss call neither this helper nor
// any inferred rejection path, leaving the desired state honestly pending.
func (attachmentState *State) recordProviderRejection(seam string, snapshot providerCausationSnapshot) {
	if attachmentState.identity.metadata == nil {
		return
	}
	attachmentState.stateMu.Lock()
	generation, nativeID := attachmentState.attachmentGeneration, attachmentState.threadID
	attachmentState.stateMu.Unlock()
	committed, failed := false, false
	for field, causal := range snapshot {
		id := causal.EventID
		appended, err := attachmentState.identity.metadata.AppendEvent(hostedstore.MetadataEvent{
			HostedThreadID: attachmentState.identity.hostedThreadID, Field: field, Value: causal.Value,
			Disposition: metadataDispositionRejected, Source: postureSourceProvider,
			Evidence: seam, NativeThreadID: nativeID, NanoSessionID: attachmentState.identity.id,
			AttachmentGeneration: generation, Freshness: metadataFreshnessLive,
			CausationEventID: &id,
			SourceEventKey:   providerEvidenceKey(attachmentState.identity.hostedThreadID, generation, seam, field, causal.Value, metadataFreshnessLive, &causal, "rejected"),
		})
		if err != nil {
			attachmentState.markMetadataDegraded()
			log.Printf("record provider rejection %s/%s: %v", attachmentState.identity.hostedThreadID, field, err)
			failed = true
		} else if appended.Committed {
			committed = true
		}
	}
	if committed || failed {
		attachmentState.NotifyRuntime()
	}
}
