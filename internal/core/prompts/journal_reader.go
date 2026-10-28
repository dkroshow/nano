package prompts

import (
	"context"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// JournalReader is the shared read surface of a writer or read-only journal.
type JournalReader interface {
	List(context.Context, string) ([]Record, error)
	ListThreadAttempts(context.Context, model.Provider, string) ([]AttemptStatus, error)
}
