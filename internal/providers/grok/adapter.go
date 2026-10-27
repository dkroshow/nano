package grok

import (
	"github.com/dkroshow/nano/internal/providers"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// Adapter is the Grok provider boundary. It stores only value-operation
// functions and qualified evidence; it never owns a hosted session.
type Adapter struct{ *providers.CheckedAdapter }

func New(operations providers.Operations) (*Adapter, error) {
	checked, err := providers.NewCheckedAdapter(model.ProviderGrok, Declaration(), operations)
	if err != nil {
		return nil, err
	}
	return &Adapter{CheckedAdapter: checked}, nil
}
