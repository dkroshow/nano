package hosted

import (
	"context"
	"time"

	"github.com/dkroshow/nano/internal/features/subscriptions"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	interactmodel "github.com/dkroshow/nano/internal/interact"
	"github.com/dkroshow/nano/internal/observability"
	ownedthreads "github.com/dkroshow/nano/internal/threads"
	threadvalues "github.com/dkroshow/nano/internal/threads"
)

// SessionView is the hosted session response, including live attachment and durable metadata reads.
type SessionView struct {
	ID               string `json:"id"`
	ClientSessionID  string `json:"clientSessionId,omitempty"`
	Provider         string `json:"provider"`
	PresentationMode string `json:"presentationMode"`
	SidebarPlacement string `json:"sidebarPlacement"`
	NanoSessionID    string `json:"nanoSessionId,omitempty"`
	NativeThreadID   string `json:"nativeThreadId,omitempty"`
	// NativeThreadIDProvisional marks a Claude shim stand-in (cs_…) that has not
	// been rekeyed to a real provider id: the thread never completed its first
	// prompt. Consumers must not resume or cite a provisional value.
	NativeThreadIDProvisional bool                             `json:"nativeThreadIdProvisional,omitempty"`
	InvokerHostedThreadID     string                           `json:"invokerHostedThreadId,omitempty"`
	InvokerNativeThreadID     string                           `json:"invokerNativeThreadId,omitempty"`
	InvokerProvider           string                           `json:"invokerProvider,omitempty"`
	Subscriptions             subscriptions.Choices            `json:"subscriptions"`
	Status                    string                           `json:"status"`
	Error                     string                           `json:"error,omitempty"`
	ThreadDiagnostics         []observability.ThreadDiagnostic `json:"threadDiagnostics,omitempty"`
	Attached                  bool                             `json:"attached"`
	Ready                     bool                             `json:"ready"`
	Working                   bool                             `json:"working"`
	TurnActive                bool                             `json:"turnActive"`
	Waiting                   bool                             `json:"waiting,omitempty"`
	Availability              string                           `json:"availability"`
	AvailabilityReasons       []string                         `json:"availabilityReasons"`
	LastHumanMessageAt        string                           `json:"lastHumanMessageAt,omitempty"`
	AttachmentState           string                           `json:"attachmentState"`
	CreatedAt                 string                           `json:"createdAt"`
	UpdatedAt                 string                           `json:"updatedAt"`
	Revision                  int64                            `json:"revision"`
	Attachment                hostedstore.AttachmentView       `json:"attachment"`
	Metadata                  *hostedstore.MetadataView        `json:"metadata,omitempty"`
	Assignment                *hostedstore.AssignmentView      `json:"assignment,omitempty"`
	AssignmentUnavailable     bool                             `json:"assignmentUnavailable,omitempty"`
	MetadataUnavailable       bool                             `json:"metadataUnavailable,omitempty"`
	MetadataDegraded          bool                             `json:"metadataDegraded,omitempty"`
}

// TurnView is the public operation response with its immutable admission and saved-result evidence.
type TurnView struct {
	ID                     string                             `json:"id"`
	HostedThreadID         string                             `json:"hostedThreadId"`
	ClientActionID         string                             `json:"clientActionId"`
	Provider               string                             `json:"provider"`
	NanoSessionID          string                             `json:"nanoSessionId,omitempty"`
	NativeThreadID         string                             `json:"nativeThreadId,omitempty"`
	ProviderTurnID         string                             `json:"providerTurnId,omitempty"`
	Status                 string                             `json:"status"`
	Terminal               bool                               `json:"terminal"`
	Error                  *ownedthreads.ExecutionError       `json:"error,omitempty"`
	FinalItemID            string                             `json:"finalItemId,omitempty"`
	FinalResponse          string                             `json:"finalResponse,omitempty"`
	FinalResponseTruncated bool                               `json:"finalResponseTruncated"`
	FinalResponseBytes     int                                `json:"finalResponseBytes,omitempty"`
	QueuePosition          int                                `json:"queuePosition,omitempty"`
	Origin                 string                             `json:"origin,omitempty"`
	SenderHostedThreadID   string                             `json:"senderHostedThreadId,omitempty"`
	SenderNativeThreadID   string                             `json:"senderNativeThreadId,omitempty"`
	SenderProvider         string                             `json:"senderProvider,omitempty"`
	SenderName             string                             `json:"senderName,omitempty"`
	SenderSubmittedAt      string                             `json:"senderSubmittedAt,omitempty"`
	Interactions           []interactmodel.PendingInteraction `json:"interactions"`
	Assignment             *hostedstore.AssignmentView        `json:"assignment,omitempty"`
	AssignmentUnavailable  bool                               `json:"assignmentUnavailable,omitempty"`
	CreatedAt              string                             `json:"createdAt"`
	UpdatedAt              string                             `json:"updatedAt"`
	Revision               int64                              `json:"revision"`
}

type executionFamilyMemberView struct {
	HostedThreadID        string   `json:"hostedThreadId"`
	Provider              string   `json:"provider"`
	NativeThreadID        string   `json:"nativeThreadId,omitempty"`
	InvokerHostedThreadID string   `json:"invokerHostedThreadId,omitempty"`
	Status                string   `json:"status"`
	OperationIDs          []string `json:"operationIds"`
}

// SessionView reads one hosted execution with its current runtime overlay.
func (s *ViewService) SessionView(id string) SessionView {
	rec, _ := s.Executions.Session(id)
	view := SessionView{
		ID: rec.ID, ClientSessionID: rec.ClientSessionID,
		Provider: rec.Provider, PresentationMode: rec.PresentationMode, SidebarPlacement: rec.SidebarPlacement,
		NanoSessionID: rec.NanoSessionID, NativeThreadID: rec.NativeThreadID,
		InvokerHostedThreadID: rec.InvokerHostedThreadID,
		InvokerNativeThreadID: rec.InvokerNativeThreadID, InvokerProvider: rec.InvokerProvider,
		Subscriptions: hostedstore.StoredSubscriptions(rec),
		Status:        rec.Status, Error: rec.Error, CreatedAt: rec.CreatedAt,
		UpdatedAt: rec.UpdatedAt, Revision: rec.Revision,
	}
	_, waiting := s.Boot.WaitingHosted(id)
	if live := s.sessionForHosted(id); live != nil {
		sum := live.Summary()
		ready, working := live.State().ReadyAndWorking()
		view.NanoSessionID, view.NativeThreadID, view.Status = sum.ID, sum.ThreadID, sum.Status
		view.Error, view.TurnActive, view.Waiting = sum.Error, sum.TurnActive, sum.Waiting
		view.ThreadDiagnostics = sum.ThreadDiagnostics
		view.MetadataDegraded = sum.MetadataDegraded
		view.Attached = true
		view.Ready, view.Working = ready, working
	} else if waiting != "" {
		view.Status = waiting
	}
	view.NativeThreadIDProvisional = hostedstore.NativeThreadIDProvisional(view.Provider, view.NativeThreadID)
	view.Attachment = s.AttachmentState(id)
	if s.Metadata != nil && s.MetadataError == "" {
		if metadata, err := s.Metadata.View(id); err == nil {
			view.Metadata = s.DecorateMetadata(metadata, view.Attachment)
		} else {
			view.MetadataUnavailable = true
		}
		if assignment, err := s.Metadata.CurrentOrchestrationAssignment(id); err == nil {
			view.Assignment = assignment
		} else {
			view.AssignmentUnavailable = true
		}
	} else {
		view.MetadataUnavailable = true
		view.AssignmentUnavailable = true
	}
	availability := s.AvailabilityFor(id, time.Now().UTC())
	view.Availability = availability.State
	view.AvailabilityReasons = availability.Reasons
	view.LastHumanMessageAt = availability.Facts.LastHumanMessageAt
	view.AttachmentState = availability.Facts.Attachment
	return view
}

// DecorateMetadata adds read-time joins that are deliberately outside
// immutable metadata events: attachment liveness and durable user-assigned
// name provenance.  Neither changes the hosted revision.
func (s *ViewService) DecorateMetadata(metadata hostedstore.MetadataView, attachment hostedstore.AttachmentView) *hostedstore.MetadataView {
	metadata.Attachment = attachment
	if s.Catalog != nil && metadata.NativeThreadID != "" {
		if ref, ok := s.Catalog.ThreadNameRefFor(context.Background(), metadata.Provider, metadata.NativeThreadID); ok && ref != nil && ref.Value != "" {
			metadata.Name = hostedstore.MetadataName{Value: ref.Value, Provenance: string(ref.Provenance)}
		}
	}
	return &metadata
}

// AttachmentState is a read-time join of process liveness and the
// normalized execution record. It deliberately does not write hosted_threads:
// attachment liveness is volatile and must not manufacture a metadata revision.
func (s *ViewService) AttachmentState(id string) hostedstore.AttachmentView {
	_, waiting := s.Boot.WaitingHosted(id)
	if live := s.sessionForHosted(id); live != nil {
		sum := live.Summary()
		attachment := hostedstore.AttachmentView{State: sum.Status, Provenance: "live_session", Error: sum.Error}
		if attachment.State == "" {
			attachment.State = "attached"
		}
		return attachment
	}
	if waiting != "" {
		return hostedstore.AttachmentView{State: waiting, Provenance: "boot_reattach"}
	}
	if s.Executions == nil {
		return hostedstore.AttachmentView{State: "unavailable", Provenance: "execution_control", Error: "execution control unavailable"}
	}
	rec, ok := s.Executions.Session(id)
	if !ok {
		return hostedstore.AttachmentView{State: "unavailable", Provenance: "execution_record", Error: "hosted execution record unavailable"}
	}
	state := rec.Status
	if state == "" {
		state = "detached"
	}
	return hostedstore.AttachmentView{State: state, Provenance: "execution_record", Error: rec.Error}
}

// TurnView reads one operation with its live interactions and assignment snapshot.
func (s *ViewService) TurnView(rec ownedthreads.TurnRecord) TurnView {
	view := TurnView{
		ID: rec.ID, HostedThreadID: rec.HostedThreadID, ClientActionID: rec.ClientActionID,
		Provider: rec.Provider, NanoSessionID: rec.NanoSessionID, NativeThreadID: rec.NativeThreadID,
		ProviderTurnID: rec.ProviderTurnID, Status: rec.Status, Terminal: threadvalues.TurnTerminal(rec.Status),
		Error: rec.Error, FinalItemID: rec.FinalItemID, FinalResponse: rec.FinalResponse,
		FinalResponseTruncated: rec.FinalResponseTruncated, FinalResponseBytes: rec.FinalResponseBytes,
		Origin: rec.Origin, SenderHostedThreadID: rec.SenderHostedThreadID, SenderNativeThreadID: rec.SenderNativeThreadID,
		SenderProvider: rec.SenderProvider, SenderName: rec.SenderName, SenderSubmittedAt: rec.SenderSubmittedAt,
		CreatedAt: rec.CreatedAt, UpdatedAt: rec.UpdatedAt, Revision: rec.Revision,
		Interactions: []interactmodel.PendingInteraction{},
	}
	runtime := s.sessionForHosted(rec.HostedThreadID)
	if runtime != nil {
		view.NanoSessionID = runtime.ID()
		if threadID := runtime.State().ThreadID(); threadID != "" {
			view.NativeThreadID = threadID
		}
		if !threadvalues.TurnTerminal(rec.Status) {
			for _, interaction := range runtime.Delivery().PendingInteractionList() {
				if interaction.TurnID == "" || rec.ProviderTurnID == "" || interaction.TurnID == rec.ProviderTurnID {
					view.Interactions = append(view.Interactions, interaction)
				}
			}
		}
	}
	if rec.Status == ownedthreads.AgentTurnQueued {
		view.QueuePosition = s.sessionForHosted(rec.HostedThreadID).Delivery().QueuePositionOr(rec.TransportActionID, s.Queue)
	}
	if s.Metadata != nil && s.MetadataError == "" {
		assignment, err := s.Metadata.OperationOrchestrationAssignment(rec.AssignmentEventID)
		if err != nil {
			view.AssignmentUnavailable = true
		} else {
			view.Assignment = assignment
		}
	} else if rec.AssignmentEventID != nil {
		view.AssignmentUnavailable = true
	}
	return view
}
