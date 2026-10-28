package app

import (
	turns "github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/hosted"
	"github.com/dkroshow/nano/internal/threads"
)

// Catalog and session composition share the hosted attachment values.
type attachmentRecord = hosted.Attachment
type attachmentKey = hosted.AttachmentKey

type agentExecutionStore = hosted.ExecutionStore
type controlMetadataStore = hosted.MetadataStore
type completedTurnStore = hosted.CompletedTurnStore
type completedTurnSource = hosted.CompletedTurnSource
type agentSessionRecord = threads.SessionRecord
type agentTurnRecord = threads.TurnRecord
type orchestrationAssignment = hosted.Assignment
type orchestrationAssignmentView = hosted.AssignmentView
type externalResumeFacts = hosted.ExternalResumeFacts
type recordReference = hosted.RecordReference
type turnAttribution = hosted.TurnAttribution
type metadataCurrentView = hosted.MetadataCurrent
type hostedMetadataView = hosted.MetadataView
type hostedAttachmentView = hosted.AttachmentView
type hostedMetadataName = hosted.MetadataName

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
