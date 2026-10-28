package app

import "github.com/dkroshow/nano/internal/threadindex"

type catalogInteractionCounts = threadindex.InteractionCounts
type transcriptEvidenceResponse = threadindex.TranscriptEvidenceResponse
type transcriptAuditReferenceRow = threadindex.AuditReferenceRow

// threadMessagesResponse checks the HTTP wire envelope independently of the
// index's private response type.
type threadMessagesResponse struct {
	ThreadID string                `json:"threadId"`
	Count    int                   `json:"count"`
	Messages []threadindex.Message `json:"messages"`
}
