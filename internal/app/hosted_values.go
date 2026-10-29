package app

import (
	turns "github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/hosted"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/threads"
)

// Catalog and session composition share the hosted attachment values.
type attachmentRecord = hostedstore.Attachment
type attachmentKey = hostedstore.AttachmentKey

type agentExecutionStore = hostedstore.ExecutionStore
type controlMetadataStore = hostedstore.MetadataStore
type completedTurnStore = hosted.CompletedTurnStore
type completedTurnSource = hosted.CompletedTurnSource
type agentSessionRecord = threads.SessionRecord
type agentTurnRecord = threads.TurnRecord
type orchestrationAssignment = hostedstore.Assignment
type orchestrationAssignmentView = hostedstore.AssignmentView
type externalResumeFacts = hostedstore.ExternalResumeFacts
type recordReference = hostedstore.RecordReference
type turnAttribution = hostedstore.TurnAttribution
type metadataCurrentView = hostedstore.MetadataCurrent
type hostedMetadataView = hostedstore.MetadataView
type hostedAttachmentView = hostedstore.AttachmentView
type hostedMetadataName = hostedstore.MetadataName

const (
	sidebarPlacementStandalone = threads.SidebarPlacementStandalone
	agentTurnCompleted         = threads.AgentTurnCompleted
)

const ()

// queuedTurnRecord is the schema-20 compatibility shape exposed by the read
// proxy below. Production delivery writes only through internal/core/prompts.Store.
// HostedThreadID is the stable pre-native identity; migrated records may carry
// only NativeThreadID.

const (
	turnOriginAgent = string(turns.AgentOrigin)
)

// queuedTurnStore is the prompt owner's compatibility read view.
type queuedTurnStore = turns.LegacyQueue
