package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/dkroshow/nano/internal/history"
	"github.com/dkroshow/nano/internal/threads"
)

type creationNameProjectionRecord struct {
	Provider, NativeThreadID, Name string
}

type creationNameProjector interface {
	PublishCreationNames(context.Context, []creationNameProjectionRecord) error
}

func (s *controlMetadataStore) recordCreationName(hostedID, name string) error {
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

func (s *controlMetadataStore) creationName(hostedID string) (string, bool) {
	if s == nil {
		return "", false
	}
	name, ok, err := threads.ReadCreationName(context.Background(), s.readPool(), hostedID)
	if err != nil {
		return "", false
	}
	return name, ok
}

func (s *server) creationNameForHosted(hostedID string) string {
	if s.controlMetadata == nil {
		return ""
	}
	name, _ := s.controlMetadata.creationName(hostedID)
	return name
}

func (s *server) publishCreationName(provider, nativeThreadID, name string) {
	projector, ok := s.catalog.(creationNameProjector)
	if !ok {
		log.Printf("publish creation name %q for %s/%s: catalog unavailable", name, provider, nativeThreadID)
		return
	}
	if err := projector.PublishCreationNames(context.Background(), []creationNameProjectionRecord{{
		Provider: provider, NativeThreadID: nativeThreadID, Name: name,
	}}); err != nil {
		log.Printf("publish creation name %q for %s/%s: %v", name, provider, nativeThreadID, err)
	}
}

func (s *controlMetadataStore) creationThreadNames(ctx context.Context) ([]history.ThreadNameRecord, error) {
	if s == nil {
		return nil, nil
	}
	rows, err := s.readPool().QueryContext(ctx, `
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
