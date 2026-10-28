package hosted

import (
	"context"
	"log"
)

// CreationNameProjection is the canonical creation name projected into the catalog.
type CreationNameProjection struct {
	Provider, NativeThreadID, Name string
}

// CreationNameProjector publishes canonical creation names into the derived catalog.
type CreationNameProjector interface {
	PublishCreationNames(context.Context, []CreationNameProjection) error
}

// Publish projects a single creation name after admission or attachment.
func (s *CreationNames) Publish(provider, nativeThreadID, name string) {
	projector := s.Projector
	if projector == nil {
		log.Printf("publish creation name %q for %s/%s: catalog unavailable", name, provider, nativeThreadID)
		return
	}
	if err := projector.PublishCreationNames(context.Background(), []CreationNameProjection{{
		Provider: provider, NativeThreadID: nativeThreadID, Name: name,
	}}); err != nil {
		log.Printf("publish creation name %q for %s/%s: %v", name, provider, nativeThreadID, err)
	}
}
