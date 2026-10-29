package sidebar

import (
	featuremail "github.com/dkroshow/nano/internal/features/mail"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/sessions"
	"github.com/dkroshow/nano/internal/threadindex"
)

// Sources are the concrete owners used to construct the visible-thread controller.
type Sources struct {
	Catalog       *threadindex.Service
	Registry      *sessions.Registry
	Executions    *hostedstore.ExecutionStore
	Metadata      *hostedstore.MetadataStore
	MetadataError string
	Attachments   *hostedstore.AttachmentStore
	Boot          *sessions.BootAttachments
	Stars         *Stars
	Mail          *featuremail.Store
	MailError     string
	RuntimeEvents *RuntimeHub
}

// Subscribe registers a browser for materialized sidebar changes.
func (s *Controller) Subscribe() chan Delta { return s.events.subscribe() }

// Unsubscribe removes a browser's sidebar change subscription.
func (s *Controller) Unsubscribe(ch chan Delta) { s.events.unsubscribe(ch) }

// SubscribeFleet registers a browser for the optional fleet transition mirror.
func (s *Controller) SubscribeFleet() (chan FleetTransition, func()) {
	if s.fleet == nil {
		return nil, func() {}
	}
	ch := s.fleet.hub.subscribe()
	return ch, func() { s.fleet.hub.unsubscribe(ch) }
}

// WarmReport reads counters from the currently open warm streams.
func (s *Controller) WarmReport() []sessions.WarmStreamReport { return s.warm.report() }
