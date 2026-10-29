package app

import (
	"github.com/dkroshow/nano/internal/threadindex"
)

// threadMessagesResponse checks the HTTP wire envelope independently of the
// index's private response type.
type threadMessagesResponse struct {
	ThreadID string                `json:"threadId"`
	Count    int                   `json:"count"`
	Messages []threadindex.Message `json:"messages"`
}
