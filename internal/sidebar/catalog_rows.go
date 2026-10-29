package sidebar

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/dkroshow/nano/internal/history"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/threadindex"
	ownedthreads "github.com/dkroshow/nano/internal/threads"
)

// MemoryThreadRow projects one live session into a catalog row. Only what the
// session actually knows becomes a known cell; the rest stays unknown per the
// three-way metadata contract — a runtime row is thin and honest about it
// rather than padded with defaults. Title and preview stay null: they are
// derived from the provider transcript, which this row does not have. A
// dialog-supplied name is session-owned until the durable overlay takes over,
// so it is safe to carry through immediately.
//
// The runtime overlay is stamped here rather than left to AttachRuntimeOverlay:
// a memory row is attached by construction, and a session still starting has no
// native id for the (provider, nativeId) lookup to match. The later overlay pass
// leaves a stamped row alone.
func MemoryThreadRow(sess sessions.Summary) Thread {
	createdAt := sess.CreatedAt
	runtime := sessionRuntimeInfo(sess)
	var name *threadindex.ThreadName
	if sess.Name != "" {
		name = &threadindex.ThreadName{
			Value: sess.Name, Provenance: "manual", Evidence: threadindex.ThreadNameEvidence,
		}
	}
	return Thread{Thread: threadindex.Thread{ID: memoryThreadIDPrefix + sess.ID, Provider: sess.Provider, NativeID: sess.ThreadID, Kind: memoryThreadKind, CreatedAt: &createdAt, UpdatedAt: &createdAt, Metadata: threadindex.Metadata{
		CWD: runtimeCatalogValue(sess.Cwd),
		// The posture cells serve the same unified projection as the
		// summary and /state (posture_view.go, decision 4) — which is what
		// lights up effort/priority/permission for a live row, and shows
		// the creation-request model before the provider's own handshake
		// reports one.
		Model:            runtimeCatalogValue(sess.Posture.Model),
		Effort:           runtimeCatalogValue(sess.Posture.Effort),
		Priority:         runtimeCatalogValue(sess.Posture.ServiceTier),
		Permission:       runtimeCatalogValue(sess.Posture.PermissionMode),
		HistoricalBranch: unknownCatalogValue(),
		Origin:           unknownCatalogValue(),
	}, Interaction: memoryThreadInteraction, Name: name, Source: threadindex.SourceSummary{}}, HostedThreadID: sess.HostedThreadID, InvokerHostedThreadID: sess.InvokerHostedThreadID, InvokerNativeThreadID: sess.InvokerNativeThreadID, InvokerProvider: sess.InvokerProvider, SidebarPlacement: sess.SidebarPlacement, Transient: true, Runtime: &runtime}
}

// runtimeCatalogValue makes one metadata cell from a live session field: known
// when the session holds the value, unknown while it is still being learned
// (claude's model arrives with the init line, after the row can first appear).
func runtimeCatalogValue(value string) threadindex.Value {
	if value == "" {
		return unknownCatalogValue()
	}
	evidence := runtimeEvidence
	return threadindex.Value{State: string(history.MetadataKnown), Value: &value, Evidence: &evidence}
}

func unknownCatalogValue() threadindex.Value {
	return threadindex.Value{State: string(history.MetadataUnknown)}
}

// AttachRuntimeOverlay stamps the volatile runtime fields and Nano's durable
// hosted-thread identity overlay onto one catalog thread. Invocation lineage
// is deliberately separate from provider-native parentNativeId: an ordinary
// Codex hosted thread may have invoked a Claude or Grok hosted thread.
func (s *Controller) AttachRuntimeOverlay(thread *Thread) {
	if s.registry != nil {
		if info, ok := s.runtimeFor(thread.Provider, thread.NativeID); ok {
			thread.Runtime = &info
		}
	}
	record, ok := s.catalogHostedThreadRecord(*thread)
	if !ok {
		return
	}
	thread.HostedThreadID = record.ID
	thread.HostedStatus = record.Status
	thread.InvokerHostedThreadID = record.InvokerHostedThreadID
	thread.InvokerNativeThreadID = record.InvokerNativeThreadID
	thread.InvokerProvider = record.InvokerProvider
	thread.SidebarPlacement = record.SidebarPlacement
	// A durable hosted overlay supersedes transcript-derived standing posture.
	// Provider files remain valuable evidence for non-hosted threads, but once
	// a hosted identity exists the control store is the only metadata authority.
	s.applyHostedCatalogMetadata(thread, record.ID)
	s.applyHostedCatalogAssignment(thread, record.ID)
}

func (s *Controller) applyHostedCatalogMetadata(thread *Thread, hostedID string) {
	setUnknown := func() {
		thread.Metadata.CWD = unknownCatalogValue()
		thread.Metadata.Model = unknownCatalogValue()
		thread.Metadata.Effort = unknownCatalogValue()
		thread.Metadata.Priority = unknownCatalogValue()
		thread.Metadata.Permission = unknownCatalogValue()
	}
	if s.controlMetadata == nil || s.controlMetadataErr != "" {
		setUnknown()
		return
	}
	view, err := s.controlMetadata.View(hostedID)
	if err != nil {
		setUnknown()
		return
	}
	canonical := func(field string) threadindex.Value {
		value, ok := hostedstore.MetadataString(view.Fields[field])
		if !ok {
			return unknownCatalogValue()
		}
		evidence := "canonical_hosted_metadata"
		return threadindex.Value{State: string(history.MetadataKnown), Value: &value, Evidence: &evidence}
	}
	thread.Metadata.CWD = canonical("cwd")
	thread.Metadata.Model = canonical(hostedstore.PostureFieldModel)
	thread.Metadata.Effort = canonical(hostedstore.PostureFieldEffort)
	thread.Metadata.Priority = canonical(hostedstore.PostureFieldServiceTier)
	thread.Metadata.Permission = canonical(hostedstore.PostureFieldPermissionMode)
}

func (s *Controller) applyHostedCatalogAssignment(thread *Thread, hostedID string) {
	thread.Assignment = nil
	thread.AssignmentUnavailable = false
	if s.controlMetadata == nil || s.controlMetadataErr != "" {
		thread.AssignmentUnavailable = true
		return
	}
	assignment, err := s.controlMetadata.CurrentOrchestrationAssignment(hostedID)
	if err != nil {
		thread.AssignmentUnavailable = true
		return
	}
	thread.Assignment = assignment
}

func (s *Controller) catalogHostedThreadRecord(thread Thread) (ownedthreads.SessionRecord, bool) {
	if s.agentExecutions == nil {
		return ownedthreads.SessionRecord{}, false
	}
	if thread.HostedThreadID != "" {
		if record, ok := s.agentExecutions.Session(thread.HostedThreadID); ok {
			return record, true
		}
	}
	return s.agentExecutions.SessionForNative(thread.Provider, thread.NativeID)
}

// AttachFleetStates stamps each row's turn state (fleet_status.go). Rows Nano
// does not hold are classified from their saved files in one batch query.
//
// Best-effort by design: a failed classification
// leaves those rows' field absent rather than failing the page.
func (s *Controller) attachFleetStates(ctx context.Context, threads []Thread) {
	courts, err := s.fleetCourts(ctx, threads)
	now := time.Now()
	for i := range threads {
		if err != nil && !nanoHolds(threads[i]) {
			continue
		}
		threads[i].Fleet = threadFleetState(threads[i], courts[threads[i].ID], now)
	}
}

// fleetCourts reads the saved-file evidence of the rows Nano does not hold.
func (s *Controller) fleetCourts(ctx context.Context, threads []Thread) (map[string]fleetCourtMessage, error) {
	source := s.catalog
	if source == nil {
		return nil, errors.New("catalog has no fleet court read")
	}
	ids := make([]string, 0, len(threads))
	for _, thread := range threads {
		if !nanoHolds(thread) {
			ids = append(ids, thread.ID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	courts, err := source.FleetCourt(ctx, ids)
	if err != nil {
		log.Printf("catalog fleet states for %d threads: %v", len(ids), err)
		return nil, err
	}
	return courts, nil
}

const (
	// MemoryThreadIDPrefix distinguishes not-yet-indexed rows.
	memoryThreadIDPrefix = "mem_"
	// MemoryThreadKind is the catalog kind of a live-only row.
	memoryThreadKind        = "thread"
	memoryThreadInteraction = "unclassified"
	runtimeEvidence         = "runtime"
)
