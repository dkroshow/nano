package sidebar

import (
	"encoding/json"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	sessionruntime "github.com/dkroshow/nano/internal/sessions"
)

// ListedSession is a live attachment with its compact displayed metadata.
type ListedSession struct {
	sessionruntime.Summary
	Metadata *listedMetadata `json:"metadata,omitempty"`
}

type listedMetadata struct {
	Revision int64                          `json:"revision"`
	Fields   map[string]listedMetadataField `json:"fields"`
}

type listedMetadataField struct {
	DisplayValue json.RawMessage `json:"displayValue,omitempty"`
}

// ListMetadata projects only the metadata displayed by a session list.
func ListMetadata(view *hostedstore.MetadataView) *listedMetadata {
	if view == nil {
		return nil
	}
	fields := make(map[string]listedMetadataField, len(view.Fields))
	for name, field := range view.Fields {
		fields[name] = listedMetadataField{DisplayValue: field.DisplayValue}
	}
	return &listedMetadata{Revision: view.Revision, Fields: fields}
}

// DetachedSessionView keeps the legacy attachment shape required to locate a
// resumable provider transport, while exposing hosted facts only through the
// listed metadata live sessions carry; the full view is a thread's own read.
// The embedded record is deliberately scrubbed at response construction; old
// JSON cwd/model/tier values are migration input, not API truth.
type DetachedSessionView struct {
	hostedstore.Attachment
	// Status is "starting" while the boot loop has not registered the thread.
	Status              string          `json:"status,omitempty"`
	Metadata            *listedMetadata `json:"metadata,omitempty"`
	MetadataUnavailable bool            `json:"metadataUnavailable,omitempty"`
}
