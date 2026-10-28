package app

import (
	"github.com/dkroshow/nano/internal/catalog"
	"github.com/dkroshow/nano/internal/threadindex"
)

type catalogValue = threadindex.Value
type catalogMetadata = threadindex.Metadata
type catalogSourceSummary = threadindex.SourceSummary
type catalogSourceDetail = threadindex.SourceDetail
type catalogThreadName = threadindex.ThreadName
type catalogIndexStatus = threadindex.IndexStatus
type transcriptChange = threadindex.TranscriptChange
type transcriptListQuery = threadindex.TranscriptQuery
type transcriptProjection = threadindex.TranscriptProjection
type transcriptPageInfo = threadindex.TranscriptPageInfo
type transcriptItem = threadindex.TranscriptItem
type transcriptTurn = threadindex.TranscriptTurn
type transcriptResponse = threadindex.TranscriptResponse
type threadTranscriptCatalog = threadindex.TranscriptCatalog
type savedTurnStatusCatalog = threadindex.SavedTurnStatusCatalog
type transcriptAuditReference = threadindex.AuditReference
type transcriptAuditScope = threadindex.AuditScope

const transcriptAuditMessages = threadindex.AuditMessages

var errTranscriptAuditReferenceChanged = threadindex.ErrAuditReferenceChanged

type ambiguousThreadReferenceError = threadindex.AmbiguousReferenceError
type threadReferenceResolver = threadindex.ReferenceResolver
type threadNameInvalidError = threadindex.NameInvalidError

const maxThreadCatalogLimit = threadindex.MaxThreadCatalogLimit
const maxThreadTranscriptLimit = threadindex.MaxThreadTranscriptLimit
const maxThreadTranscriptBytes = threadindex.MaxThreadTranscriptBytes
const defaultThreadTranscriptView = threadindex.DefaultThreadTranscriptView
const devThreadTranscriptView = threadindex.DevThreadTranscriptView
const catalogEventHeartbeat = threadindex.CatalogEventHeartbeat
const maxThreadNameRequestBytes = threadindex.MaxThreadNameRequestBytes

var errThreadCatalogUnavailable = threadindex.ErrThreadCatalogUnavailable
var errCatalogThreadNotFound = threadindex.ErrCatalogThreadNotFound

const threadNameEvidence = threadindex.ThreadNameEvidence

var errInvalidThreadCatalogCursor = threadindex.ErrInvalidThreadCatalogCursor

type ingestStatus = threadindex.IngestStatus
type catalogListQuery = threadindex.ListQuery

type catalogChange = catalog.Change

type threadCatalogNamer = threadindex.Namer

type threadMessagesQuery = threadindex.MessagesQuery

const defaultThreadMessagesTail = threadindex.DefaultMessagesTail

const maxThreadMessagesTail = threadindex.MaxMessagesTail

const threadMessagesFormatText = threadindex.MessagesFormatText

const threadMessagesFormatJSON = threadindex.MessagesFormatJSON

const threadMessageRoleUser = threadindex.MessageRoleUser

const threadMessageRoleAgent = threadindex.MessageRoleAgent
