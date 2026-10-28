package app

import "github.com/dkroshow/nano/internal/threadindex"

// threadMessagesResponse checks the HTTP wire envelope independently of the
// index's private response type.
type threadMessagesResponse struct {
	ThreadID string                `json:"threadId"`
	Count    int                   `json:"count"`
	Messages []threadindex.Message `json:"messages"`
}

// Compatibility names for the hosted tests that move in the other lane.
type catalogThreadName = threadindex.ThreadName
type catalogIndexStatus = threadindex.IndexStatus
type transcriptItem = threadindex.TranscriptItem
type transcriptResponse = threadindex.TranscriptResponse

const defaultThreadTranscriptView = threadindex.DefaultThreadTranscriptView
const devThreadTranscriptView = threadindex.DevThreadTranscriptView
const threadNameEvidence = threadindex.ThreadNameEvidence
