package sessions

import (
	"database/sql"
	"encoding/json"
	"sync"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
)

// recordingMetadata echoes adapter evidence at the metadata boundary. It does
// not model the hosted store's precedence, persistence, or admission rules.
type recordingMetadata struct {
	Metadata
	mu          sync.Mutex
	db          *sql.DB
	view        hostedstore.MetadataView
	permissions []hostedstore.PermissionEvent
}

func newRecordingMetadata(db *sql.DB, hostedID string) *recordingMetadata {
	return &recordingMetadata{db: db, view: hostedstore.MetadataView{HostedThreadID: hostedID, Provider: "claude", AttachmentGeneration: 1, Fields: map[string]hostedstore.MetadataCurrent{}}}
}
func (m *recordingMetadata) AppendEvent(event hostedstore.MetadataEvent) (hostedstore.MetadataEvent, error) {
	value, err := json.Marshal(event.Value)
	if err != nil {
		return event, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.view.Revision++
	event.ID = m.view.Revision
	event.Committed = true
	m.view.Fields[event.Field] = hostedstore.MetadataCurrent{DisplayValue: value, DisplayDisposition: event.Disposition, DisplaySource: event.Source, DisplayFreshness: event.Freshness}
	return event, nil
}
func (m *recordingMetadata) View(string) (hostedstore.MetadataView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	view := m.view
	view.Fields = make(map[string]hostedstore.MetadataCurrent, len(m.view.Fields))
	for key, value := range m.view.Fields {
		view.Fields[key] = value
	}
	return view, nil
}
func (m *recordingMetadata) Posture(id string) hostedstore.Posture {
	view, _ := m.View(id)
	return hostedstore.PostureFromFields(view.Fields)
}
func (m *recordingMetadata) ClaimNativeID(_ string, _ string, native string, _ bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.view.NativeThreadID = native
	return nil
}
func (m *recordingMetadata) ClaimNativeIDResolving(id, provider, native string, provisional bool, _ *hostedstore.NativeResolution) error {
	return m.ClaimNativeID(id, provider, native, provisional)
}
func (m *recordingMetadata) CanonicalDatabase() *sql.DB { return m.db }
func (m *recordingMetadata) ReadPool() *sql.DB          { return m.db }

func (m *recordingMetadata) AppendPermissionEvent(event hostedstore.PermissionEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.permissions = append(m.permissions, event)
	return nil
}
