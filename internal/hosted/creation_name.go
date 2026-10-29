package hosted

import (
	"context"
	"log"

	"github.com/dkroshow/nano/internal/threadindex"
)

// Publish projects a single creation name after admission or attachment.
func (s *CreationNames) Publish(provider, nativeThreadID, name string) {
	projector := s.Catalog
	if projector == nil {
		log.Printf("publish creation name %q for %s/%s: catalog unavailable", name, provider, nativeThreadID)
		return
	}
	if err := projector.PublishCreationNames(context.Background(), []threadindex.CreationNameRecord{{
		Provider: provider, NativeThreadID: nativeThreadID, Name: name,
	}}); err != nil {
		log.Printf("publish creation name %q for %s/%s: %v", name, provider, nativeThreadID, err)
	}
}
