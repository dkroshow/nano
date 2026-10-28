package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/hosted"
)

// posture_view.go — the one posture struct every surface serves (coordination
// plan, decision 4): /api/sessions summaries, /state, and the catalog's
// memory rows all project from sessionPosture instead of hand-assembling
// disjoint field sets. Values come from control.sqlite's immutable hosted
// event log and current-state projection; live transport and retired JSON are
// never a fallback for a hosted identity.

type sessionPosture = hosted.Posture

// overlayFold writes the folded decision for one field over dst, leaving dst
// alone when the log holds no decision for it.
func overlayFold(folded map[string]postureEntry, field string, dst *string) {
	if e, ok := folded[field]; ok {
		*dst = e.Value
	}
}

// posture is the canonical hosted-thread display projection. Legacy JSON is a
// migration input only; unavailable metadata stays unknown rather than being
// silently reconstructed from the retired posture log or local defaults.
func (sess *session) posture() sessionPosture {
	if sess.metadata == nil {
		return sessionPosture{}
	}
	return sess.metadata.Posture(sess.hostedThreadID)
}

// markMetadataDegraded reports that a newer observation did not enter the
// canonical log. The last committed record remains readable and is still the
// only value authority; this flag is an ingestion warning, not a second
// availability decision.
func (sess *session) markMetadataDegraded() {
	sess.stateMu.Lock()
	sess.metadataDegraded = true
	sess.stateMu.Unlock()
}

// recordPosture is the one live mutation adapter.  It writes immutable events
// to control.sqlite only; canonical posture store is a read-only migration and
// rollback parser and is never dual-written by production code.
func (sess *session) recordPosture(entries []postureEntry, freshness string) {
	if sess.metadata == nil {
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
		appended, err := sess.metadata.AppendEvent(metadataEvent{
			HostedThreadID: sess.hostedThreadID, Field: entry.Field, Value: entry.Value,
			Disposition: disposition, Source: entry.Source, Evidence: entry.Evidence,
			NativeThreadID: sess.ThreadID(), NanoSessionID: sess.id,
			AttachmentGeneration: sess.attachmentGeneration, Freshness: freshness,
		})
		if err != nil {
			sess.markMetadataDegraded()
			failed = true
			log.Printf("record canonical posture %s/%s: %v", sess.hostedThreadID, entry.Field, err)
		} else if appended.Committed {
			committed = true
		}
	}
	// The process-local runtime stream is intentionally only a wake-up hint.
	// Every open tab re-fetches /state, whose hosted metadata view and revision
	// are authoritative even if a notification is coalesced or lost.
	if committed || failed {
		sess.notifyRuntime()
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
func (sess *session) snapshotProviderCausation(values map[string]string) providerCausationSnapshot {
	if sess.metadata == nil || len(values) == 0 {
		return nil
	}
	view, err := sess.metadata.View(sess.hostedThreadID)
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
// id lets the error path remove the exact request rather than leaving stale
// causation for a later provider notification.
func (sess *session) beginTurnCausation(values map[string]string) int64 {
	snapshot := sess.snapshotProviderCausation(values)
	if len(snapshot) == 0 {
		return 0
	}
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	sess.turnCausationSeq++
	id := sess.turnCausationSeq
	sess.pendingTurnCausation = append(sess.pendingTurnCausation, turnCausationEntry{ID: id, Snapshot: snapshot})
	return id
}

func (sess *session) abandonTurnCausation(id int64) {
	if id == 0 {
		return
	}
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	for i, entry := range sess.pendingTurnCausation {
		if entry.ID == id {
			sess.pendingTurnCausation = append(sess.pendingTurnCausation[:i], sess.pendingTurnCausation[i+1:]...)
			return
		}
	}
}

// takeSettingsCausation either consumes the next outbound turn request or,
// for a provider retry, returns the previous entry for this same semantic
// payload.  A second queued request always wins over retry dedupe, even if its
// settings are byte-for-byte identical.
func (sess *session) takeSettingsCausation(semanticKey string) (turnCausationEntry, bool) {
	sess.stateMu.Lock()
	defer sess.stateMu.Unlock()
	if len(sess.pendingTurnCausation) > 0 {
		entry := sess.pendingTurnCausation[0]
		sess.pendingTurnCausation = sess.pendingTurnCausation[1:]
		if sess.lastSettingsEvidence == nil {
			sess.lastSettingsEvidence = map[string]turnCausationEntry{}
		}
		sess.lastSettingsEvidence[semanticKey] = entry
		return entry, true
	}
	entry, ok := sess.lastSettingsEvidence[semanticKey]
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
func (sess *session) recordProviderEvidence(seam string, values map[string]string, freshness string, snapshot providerCausationSnapshot) {
	if sess.metadata == nil || len(values) == 0 {
		return
	}
	sess.stateMu.Lock()
	generation := sess.attachmentGeneration
	nativeID := sess.threadID
	sess.stateMu.Unlock()
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
			appended, err := sess.metadata.AppendEvent(metadataEvent{
				HostedThreadID: sess.hostedThreadID, Field: field, Value: value,
				Disposition: disposition, Source: postureSourceProvider, Evidence: seam,
				NativeThreadID: nativeID, NanoSessionID: sess.id,
				AttachmentGeneration: generation, Freshness: freshness,
				CausationEventID: causeID,
				SourceEventKey:   providerEvidenceKey(sess.hostedThreadID, generation, seam, field, actual, freshness, cause, kind),
			})
			if err != nil {
				sess.markMetadataDegraded()
				failed = true
				log.Printf("record provider evidence %s/%s: %v", sess.hostedThreadID, field, err)
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
		sess.notifyRuntime()
	}
}

// recordProviderRejection settles only the exact outbound requests a provider
// explicitly rejected. Timeouts and transport loss call neither this helper nor
// any inferred rejection path, leaving the desired state honestly pending.
func (sess *session) recordProviderRejection(seam string, snapshot providerCausationSnapshot) {
	if sess.metadata == nil {
		return
	}
	sess.stateMu.Lock()
	generation, nativeID := sess.attachmentGeneration, sess.threadID
	sess.stateMu.Unlock()
	committed, failed := false, false
	for field, causal := range snapshot {
		id := causal.EventID
		appended, err := sess.metadata.AppendEvent(metadataEvent{
			HostedThreadID: sess.hostedThreadID, Field: field, Value: causal.Value,
			Disposition: metadataDispositionRejected, Source: postureSourceProvider,
			Evidence: seam, NativeThreadID: nativeID, NanoSessionID: sess.id,
			AttachmentGeneration: generation, Freshness: metadataFreshnessLive,
			CausationEventID: &id,
			SourceEventKey:   providerEvidenceKey(sess.hostedThreadID, generation, seam, field, causal.Value, metadataFreshnessLive, &causal, "rejected"),
		})
		if err != nil {
			sess.markMetadataDegraded()
			log.Printf("record provider rejection %s/%s: %v", sess.hostedThreadID, field, err)
			failed = true
		} else if appended.Committed {
			committed = true
		}
	}
	if committed || failed {
		sess.notifyRuntime()
	}
}

// threadNameReader is the optional read half of the names surface. The names
// store keeps its own truth file (decision 1: names are joined at read time,
// not copied into the posture log); this is how a resumed session picks its
// durable name back up for the boot summary.
type threadNameReader interface {
	ThreadNameFor(ctx context.Context, provider, nativeThreadID string) (string, bool)
}

type threadNameRefReader interface {
	ThreadNameRefFor(ctx context.Context, provider, nativeThreadID string) (*history.ThreadNameRef, bool)
}

// durableThreadName reads the durable user-assigned name for a thread, ""
// when unnamed or when the catalog cannot answer.
func (s *server) durableThreadName(provider, nativeThreadID string) (string, bool) {
	reader, ok := s.catalog.(threadNameReader)
	if !ok || nativeThreadID == "" {
		return "", false
	}
	return reader.ThreadNameFor(context.Background(), provider, nativeThreadID)
}

func (s *server) durableThreadNameRef(provider, nativeThreadID string) (hostedMetadataName, bool) {
	reader, ok := s.catalog.(threadNameRefReader)
	if !ok || nativeThreadID == "" {
		return hostedMetadataName{}, false
	}
	ref, ok := reader.ThreadNameRefFor(context.Background(), provider, nativeThreadID)
	if !ok || ref == nil || ref.Value == "" {
		return hostedMetadataName{}, false
	}
	return hostedMetadataName{Value: ref.Value, Provenance: string(ref.Provenance)}, true
}
