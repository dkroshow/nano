package hosted

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dkroshow/nano/internal/config"
	"github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/diagnostics"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/inspection"
	"github.com/dkroshow/nano/internal/interact"
	"github.com/dkroshow/nano/internal/observability"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/setup"
	"github.com/dkroshow/nano/internal/sidebar"
	"github.com/dkroshow/nano/internal/threadindex"
)

// AttachmentSources supplies the concrete owners used to register attachments.
// SweepSubscriptions publishes completion changes to the subscription owner.
type AttachmentSources struct {
	Validations                   *observability.ValidationStore
	Defaults                      config.SessionDefaults
	Codex                         *sessions.CodexDaemon
	SpoolDir                      string
	Catalog                       *threadindex.Service
	CatalogRefresh                func(string)
	CatalogSources                sessions.AttachedCatalog
	Runtime                       *sidebar.Controller
	Registry                      *sessions.Registry
	Executions                    *hostedstore.ExecutionStore
	Metadata                      *hostedstore.MetadataStore
	MetadataError, ExecutionError string
	Attachments                   *hostedstore.AttachmentStore
	Queue                         *prompts.LegacyQueue
	Turns                         *prompts.Store
	SendAttempts                  *prompts.LegacyJournal
	CompletedTurns                *CompletedTurnSource
	Skills                        *setup.SkillsListCache
	Notices                       *interact.Notices
	Stopping                      *atomic.Bool
	DeliveryExpiries              *sessions.DeliveryExpiries
	Restart                       *RestartTracker
	Boot                          *sessions.BootAttachments
	Diagnostics                   *diagnostics.Store
	CreationNames                 *CreationNames
	ProcessStarted                time.Time
	DropLiveTextDeltas            bool
	SweepSubscriptions            func(string)
}

// AttachmentCoordinator owns boot registration and explicit attachment attempts.
// It serializes resumes per provider and uses the same spawn path for every entry.
type AttachmentCoordinator struct {
	claudeOwedRuns sync.Map
	AttachmentSources
	boot                                                                *bootReattach
	resumeSpawnMu, resumeSpawnClaude, resumeSpawnCodex, resumeSpawnGrok sync.Mutex
}

// NewAttachmentCoordinator binds the owners used for this process's attachments.
func NewAttachmentCoordinator(sources AttachmentSources) *AttachmentCoordinator {
	return &AttachmentCoordinator{AttachmentSources: sources}
}

func (s *AttachmentCoordinator) creationNameForHosted(id string) string {
	if s.Metadata == nil {
		return ""
	}
	name, _ := s.Metadata.CreationName(id)
	return name
}
func (s *AttachmentCoordinator) durableThreadName(provider, native string) (string, bool) {
	if s.Catalog == nil || native == "" {
		return "", false
	}
	return s.Catalog.ThreadNameFor(context.Background(), provider, native)
}
func (s *AttachmentCoordinator) durableThreadNameRef(provider, native string) (hostedstore.MetadataName, bool) {
	if s.Catalog == nil || native == "" {
		return hostedstore.MetadataName{}, false
	}
	ref, ok := s.Catalog.ThreadNameRefFor(context.Background(), provider, native)
	if !ok || ref == nil || ref.Value == "" {
		return hostedstore.MetadataName{}, false
	}
	return hostedstore.MetadataName{Value: ref.Value, Provenance: string(ref.Provenance)}, true
}
func (s *AttachmentCoordinator) assignSpawnName(provider, native, name string) {
	if s.Catalog == nil {
		log.Printf("session name %q for %s/%s dropped: catalog unavailable", name, provider, native)
		return
	}
	if err := s.Catalog.AssignThreadNameIfEmpty(provider, native, name); err != nil {
		log.Printf("session name %q for %s/%s: %v", name, provider, native, err)
	}
}
func knownAttachmentValue(value threadindex.Value) string {
	return inspection.KnownMetadataValue(value.State, value.Value)
}
