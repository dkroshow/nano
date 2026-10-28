package app

import (
	"fmt"

	appstate "github.com/dkroshow/nano/internal/app/persistence"
	"github.com/dkroshow/nano/internal/hosted"
)

// AppendPermissionEvent persists one provider permission observation.
func (s *controlMetadataStore) AppendPermissionEvent(event hosted.PermissionEvent) error {
	if s == nil {
		return fmt.Errorf("canonical metadata store unavailable")
	}
	return appstate.AppendPermissionEvent(s.db, event)
}

func (s *controlMetadataStore) permissionEventsAfter(hostedID string, after int64, limit int) (hosted.PermissionEventPage, error) {
	if s == nil {
		return hosted.PermissionEventPage{}, fmt.Errorf("canonical metadata store unavailable")
	}
	return appstate.PermissionEventsAfter(s.db, hostedID, after, limit)
}
