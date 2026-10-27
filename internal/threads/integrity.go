package threads

import (
	"database/sql"
	"fmt"
)

// CheckStateIntegrity validates references among thread-owned tables.
func CheckStateIntegrity(db *sql.DB) error {
	var invalid int
	err := db.QueryRow(`SELECT count(*) FROM thread_metadata_current c
		LEFT JOIN thread_metadata_events d ON d.event_id=c.desired_event_id AND d.hosted_thread_id=c.hosted_thread_id AND d.field=c.field
		LEFT JOIN thread_metadata_events s ON s.event_id=c.desired_settlement_event_id AND s.hosted_thread_id=c.hosted_thread_id AND s.field=c.field
		LEFT JOIN thread_metadata_events e ON e.event_id=c.effective_event_id AND e.hosted_thread_id=c.hosted_thread_id AND e.field=c.field
		WHERE (c.desired_event_id IS NOT NULL AND d.event_id IS NULL) OR (c.desired_settlement_event_id IS NOT NULL AND s.event_id IS NULL) OR (c.effective_event_id IS NOT NULL AND e.event_id IS NULL)`).Scan(&invalid)
	if err != nil {
		return err
	}
	if invalid != 0 {
		return fmt.Errorf("control metadata integrity failure: %d current rows point outside their thread/field", invalid)
	}
	err = db.QueryRow(`SELECT count(*) FROM hosted_turn_operations o
		LEFT JOIN thread_metadata_events e ON e.event_id=o.assignment_event_id AND e.hosted_thread_id=o.hosted_thread_id AND e.field=?
		WHERE o.assignment_event_id IS NOT NULL AND e.event_id IS NULL`, "orchestrationAssignment").Scan(&invalid)
	if err != nil {
		return err
	}
	if invalid != 0 {
		return fmt.Errorf("control metadata integrity failure: %d operations point outside their assignment evidence", invalid)
	}
	return nil
}
