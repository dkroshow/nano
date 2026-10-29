package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/threads"
)

func (s *MetadataStore) recordCreationName(hostedID, name string) error {
	validated, err := history.ValidateThreadName(name)
	if err != nil {
		return err
	}
	revision, changed, err := threads.RecordCreationName(context.Background(), s.db, hostedID, validated, "creation_request")
	if err != nil {
		return err
	}
	if changed {
		s.publishChange(hostedID, revision, threads.CreationNameField)
	}
	return nil
}

// CreationName reads the canonical name accepted during hosted creation.
func (s *MetadataStore) CreationName(hostedID string) (string, bool) {
	if s == nil {
		return "", false
	}
	name, ok, err := threads.ReadCreationName(context.Background(), s.ReadPool(), hostedID)
	if err != nil {
		return "", false
	}
	return name, ok
}

// CreationThreadNames reads canonical creation names for projection into the catalog.
func (s *MetadataStore) CreationThreadNames(ctx context.Context) ([]history.ThreadNameRecord, error) {
	if s == nil {
		return nil, nil
	}
	rows, err := s.ReadPool().QueryContext(ctx, `
SELECT h.provider,h.current_native_id,c.effective_value_json,c.updated_at
FROM hosted_threads h
JOIN hosted_thread_native_ids n
  ON n.hosted_thread_id=h.hosted_thread_id
 AND n.provider=h.provider
 AND n.native_thread_id=h.current_native_id
 AND n.retired_at IS NULL
 AND n.provisional=0
JOIN thread_metadata_current c
  ON c.hosted_thread_id=h.hosted_thread_id AND c.field=?
WHERE h.current_native_id IS NOT NULL AND h.current_native_id<>''
  AND c.effective_disposition='confirmed'
  AND c.effective_source='creation'
ORDER BY h.provider,h.current_native_id`, threads.CreationNameField)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := []history.ThreadNameRecord{}
	for rows.Next() {
		var provider, nativeID, rawName, updated string
		if err := rows.Scan(&provider, &nativeID, &rawName, &updated); err != nil {
			return nil, err
		}
		var name string
		if err := json.Unmarshal([]byte(rawName), &name); err != nil {
			return nil, fmt.Errorf("decode creation name for %s/%s: %w", provider, nativeID, err)
		}
		validated, err := history.ValidateThreadName(name)
		if err != nil {
			return nil, fmt.Errorf("invalid creation name for %s/%s: %w", provider, nativeID, err)
		}
		stamp, err := time.Parse(time.RFC3339Nano, updated)
		if err != nil {
			return nil, fmt.Errorf("invalid creation-name timestamp for %s/%s: %w", provider, nativeID, err)
		}
		records = append(records, history.ThreadNameRecord{
			Provider: history.Provider(provider), NativeThreadID: nativeID,
			Name: validated, Provenance: history.ThreadNameCreation, UpdatedAt: &stamp,
		})
	}
	return records, rows.Err()
}

func validateCreationName(name string) (string, error) {
	validated, err := history.ValidateThreadName(name)
	if err != nil {
		return "", fmt.Errorf("invalid creation name: %w", err)
	}
	return validated, nil
}
