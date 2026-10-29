package store

import (
	"database/sql"
	"encoding/json"
	"strings"
)

// CurrentAssignmentsByHostedIDs reads current assignments in bounded database batches.
func (s *MetadataStore) CurrentAssignmentsByHostedIDs(ids []string) map[string]*AssignmentView {
	out := map[string]*AssignmentView{}
	if s == nil || len(ids) == 0 {
		return out
	}
	// control.sqlite intentionally has one open connection. Resolve audit
	// target existence before opening the assignment rows so projection never
	// attempts a nested query while that sole connection is occupied.
	hostedThreadIDs := map[string]bool{}
	hostedRows, err := s.db.Query(`SELECT hosted_thread_id FROM hosted_threads`)
	if err != nil {
		return out
	}
	for hostedRows.Next() {
		var hostedID string
		if err := hostedRows.Scan(&hostedID); err != nil {
			hostedRows.Close()
			return out
		}
		hostedThreadIDs[hostedID] = true
	}
	if err := hostedRows.Err(); err != nil {
		hostedRows.Close()
		return out
	}
	hostedRows.Close()

	unique := uniqueHostedIDs(ids)
	for start := 0; start < len(unique); start += 400 {
		end := start + 400
		if end > len(unique) {
			end = len(unique)
		}
		batch := unique[start:end]
		placeholders := make([]string, len(batch))
		args := make([]any, 0, len(batch)+1)
		args = append(args, AssignmentField)
		for i, id := range batch {
			placeholders[i] = "?"
			args = append(args, id)
		}
		rows, err := s.db.Query(`SELECT hosted_thread_id,desired_event_id,desired_disposition,desired_value_json
			FROM thread_metadata_current WHERE field=? AND hosted_thread_id IN (`+strings.Join(placeholders, ",")+`)`, args...)
		if err != nil {
			return out
		}
		for rows.Next() {
			var hostedID, disposition string
			var eventID sql.NullInt64
			var raw sql.NullString
			if err := rows.Scan(&hostedID, &eventID, &disposition, &raw); err != nil {
				rows.Close()
				return out
			}
			if disposition == metadataDispositionCleared || !eventID.Valid || !raw.Valid {
				continue
			}
			var assignment Assignment
			if err := json.Unmarshal([]byte(raw.String), &assignment); err != nil {
				continue
			}
			for i := range assignment.AuditTargets {
				assignment.AuditTargets[i].Unresolved = !hostedThreadIDs[assignment.AuditTargets[i].HostedThreadID]
			}
			out[hostedID] = &AssignmentView{
				EventID: eventID.Int64, AssignmentRole: assignment.AssignmentRole, Focus: assignment.Focus,
				RelatedHostedThreadID: assignment.RelatedHostedThreadID, AuditTargets: assignment.AuditTargets,
			}
		}
		rows.Close()
	}
	return out
}

// CurrentMetadataByHostedIDs reads canonical metadata fields in bounded database batches.
func (s *MetadataStore) CurrentMetadataByHostedIDs(ids []string) map[string]map[string]MetadataCurrent {
	if s == nil || len(ids) == 0 {
		return nil
	}
	views := map[string]map[string]MetadataCurrent{}
	unique := uniqueHostedIDs(ids)
	for start := 0; start < len(unique); start += 400 {
		end := start + 400
		if end > len(unique) {
			end = len(unique)
		}
		batch := unique[start:end]
		placeholders := make([]string, len(batch))
		args := make([]any, len(batch))
		for i, id := range batch {
			placeholders[i] = "?"
			args[i] = id
		}
		rows, err := s.db.Query(`SELECT c.hosted_thread_id,c.field,c.desired_value_json,c.desired_disposition,c.desired_source,c.desired_event_id,c.effective_value_json,c.effective_disposition,c.effective_source,c.effective_event_id,
			COALESCE(d.evidence_freshness,''),COALESCE(e.evidence_freshness,'')
			FROM thread_metadata_current c
			LEFT JOIN thread_metadata_events d ON d.event_id=c.desired_event_id
			LEFT JOIN thread_metadata_events e ON e.event_id=c.effective_event_id
			WHERE c.hosted_thread_id IN (`+strings.Join(placeholders, ",")+`)`, args...)
		if err != nil {
			return nil
		}
		for rows.Next() {
			var hostedID, field string
			var dVal, dDisp, dSource, eVal, eDisp, eSource sql.NullString
			var desiredFreshness, effectiveFreshness string
			var dID, eID sql.NullInt64
			if err := rows.Scan(&hostedID, &field, &dVal, &dDisp, &dSource, &dID, &eVal, &eDisp, &eSource, &eID, &desiredFreshness, &effectiveFreshness); err != nil {
				rows.Close()
				return nil
			}
			entry := MetadataCurrent{DesiredDisposition: nonnullDisposition(dDisp), DesiredSource: dSource.String, EffectiveDisposition: nonnullDisposition(eDisp), EffectiveSource: eSource.String}
			if dVal.Valid {
				entry.DesiredValue = json.RawMessage(dVal.String)
			}
			if dID.Valid {
				id := dID.Int64
				entry.DesiredEventID = &id
			}
			if eVal.Valid {
				entry.EffectiveValue = json.RawMessage(eVal.String)
			}
			if eID.Valid {
				id := eID.Int64
				entry.EffectiveEventID = &id
			}
			projectMetadataDisplay(&entry, desiredFreshness, effectiveFreshness)
			fields := views[hostedID]
			if fields == nil {
				fields = map[string]MetadataCurrent{}
				views[hostedID] = fields
			}
			fields[field] = entry
		}
		rows.Close()
	}
	return views
}

func uniqueHostedIDs(ids []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
