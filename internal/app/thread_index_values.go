package app

import (
	"github.com/dkroshow/nano/internal/threadindex"
)

type threadTranscriptCatalog = threadindex.TranscriptCatalog

type ambiguousThreadReferenceError = threadindex.AmbiguousReferenceError

const maxThreadCatalogLimit = threadindex.MaxThreadCatalogLimit
const maxThreadTranscriptBytes = threadindex.MaxThreadTranscriptBytes
const maxThreadNameRequestBytes = threadindex.MaxThreadNameRequestBytes

var errCatalogThreadNotFound = threadindex.ErrCatalogThreadNotFound

type catalogListQuery = threadindex.ListQuery

type threadMessagesQuery = threadindex.MessagesQuery

const defaultThreadMessagesTail = threadindex.DefaultMessagesTail

const maxThreadMessagesTail = threadindex.MaxMessagesTail

const threadMessagesFormatText = threadindex.MessagesFormatText

const threadMessagesFormatJSON = threadindex.MessagesFormatJSON

const threadMessageRoleUser = threadindex.MessageRoleUser

const threadMessageRoleAgent = threadindex.MessageRoleAgent
