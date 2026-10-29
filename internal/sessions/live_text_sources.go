package sessions

import (
	"github.com/dkroshow/nano/internal/diagnostics"
	"github.com/dkroshow/nano/internal/threadindex"
)

// LiveTextSources identifies the existing saved-read and diagnostic owners.
type LiveTextSources struct {
	Catalog        LiveTextCatalog
	References     threadindex.ReferenceResolver
	Transcripts    threadindex.TranscriptCatalog
	DeferredReread func(string) bool
	Diagnostics    *diagnostics.Store
}

// NewLiveTextLoop constructs the ordered saved/live view for one attachment.
func NewLiveTextLoop(sources LiveTextSources, sess *Session) *LiveTextLoop {
	return &LiveTextLoop{sources: sources, sess: sess, wake: make(chan struct{}, 1)}
}
