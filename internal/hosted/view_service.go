package hosted

import (
	"time"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/interact"
	"github.com/dkroshow/nano/internal/lifecycle"
	"github.com/dkroshow/nano/internal/observability"
	ownedthreads "github.com/dkroshow/nano/internal/threads"
)

// SessionSnapshot carries only the live facts used by hosted session views.
type SessionSnapshot struct {
	ID, ThreadID, Status, Error                           string
	TurnActive, Waiting, MetadataDegraded, Ready, Working bool
	ThreadDiagnostics                                     []observability.ThreadDiagnostic
}

// TurnSnapshot carries a live attachment's identity and requested interaction list.
type TurnSnapshot struct {
	ID, ThreadID string
	Interactions []interact.PendingInteraction
}

// AvailabilitySnapshot carries the live facts used by hosted availability policy.
type AvailabilitySnapshot struct {
	Status                          string
	Active, Waiting, PromptInFlight bool
	Permission                      lifecycle.Permission
	LastHumanPromptAt               time.Time
	QueuedPrompts                   int
}

// ViewSessions supplies bounded attachment reads without exposing session state or locks.
type ViewSessions interface {
	// Session reads the attachment fields used by a hosted session view.
	Session(string) (SessionSnapshot, bool)
	// Turn reads attachment identity and, when requested, pending interactions.
	Turn(string, bool) (TurnSnapshot, bool)
	// Attachment reads the live attachment status for metadata decoration.
	Attachment(string) (hostedstore.AttachmentView, bool)
	// Availability reads the runtime facts used by availability policy.
	Availability(string) (AvailabilitySnapshot, bool)
	// HostedIDs lists the hosted identities of current attachments.
	HostedIDs() []string
	// QueuePosition reads the attachment queue or the canonical fallback queue.
	QueuePosition(string, string) int
}

// ViewNames supplies the independent durable-name overlay for hosted metadata.
type ViewNames interface {
	// DurableName reads the value-only name contract.
	DurableName(string, string) (string, bool)
	// DurableNameRef reads the name and its provenance.
	DurableNameRef(string, string) (hostedstore.MetadataName, bool)
}

// ViewService assembles hosted read views from canonical facts and bounded runtime reads.
// The app supplies current dependency references for each read.
type ViewService struct {
	Executions                    *hostedstore.ExecutionStore
	Metadata                      *hostedstore.MetadataStore
	ExecutionError, MetadataError string
	Sessions                      ViewSessions
	Names                         ViewNames
	Waiting                       func(string) string
	ResolveRecord                 func(string) (ownedthreads.SessionRecord, bool)
}
