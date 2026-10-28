package hosted

import (
	"fmt"
)

// AppendPermissionEvent persists one provider permission observation.
func (s *MetadataStore) AppendPermissionEvent(event PermissionEvent) error {
	if s == nil {
		return fmt.Errorf("canonical metadata store unavailable")
	}
	return appendPermissionEvent(s.db, event)
}

// permissionEventsAfter reads persisted permission observations after an event cursor.
func (s *MetadataStore) permissionEventsAfter(hostedID string, after int64, limit int) (PermissionEventPage, error) {
	if s == nil {
		return PermissionEventPage{}, fmt.Errorf("canonical metadata store unavailable")
	}
	return permissionEventsAfter(s.db, hostedID, after, limit)
}
