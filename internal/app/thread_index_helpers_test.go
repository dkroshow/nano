package app

import "github.com/dkroshow/nano/internal/threadindex"

func renderThreadMessagesText(rows []threadMessage) string {
	values := make([]threadindex.Message, len(rows))
	for i, row := range rows {
		values[i] = row.Message
	}
	return threadindex.RenderThreadMessagesText(values)
}

type catalogInteractionCounts = threadindex.InteractionCounts
type transcriptEvidenceResponse = threadindex.TranscriptEvidenceResponse
type transcriptAuditReferenceRow = threadindex.AuditReferenceRow
type threadMessagesResponse = threadindex.MessagesResponse
