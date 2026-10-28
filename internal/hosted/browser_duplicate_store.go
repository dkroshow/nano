package hosted

import (
	"errors"

	"github.com/dkroshow/nano/internal/diagnostics"
)

// ObserveBrowserDuplicateIncident records duplicate browser attachment evidence for operator inspection.
func (s *MetadataStore) ObserveBrowserDuplicateIncident(observation diagnostics.BrowserDuplicateIncident) (diagnostics.BrowserDuplicateIncident, bool, error) {
	if s == nil {
		return diagnostics.BrowserDuplicateIncident{}, false, errors.New("durable browser duplicate store unavailable")
	}
	return diagnostics.ObserveBrowserDuplicateIncident(s.db, observation)
}

// ListBrowserDuplicateIncidents reads bounded duplicate browser attachment evidence.
func (s *MetadataStore) ListBrowserDuplicateIncidents(filter diagnostics.BrowserDuplicateIncidentFilter) ([]diagnostics.BrowserDuplicateIncident, error) {
	if s == nil {
		return nil, errors.New("durable browser duplicate store unavailable")
	}
	return diagnostics.ListBrowserDuplicateIncidents(s.db, filter)
}
