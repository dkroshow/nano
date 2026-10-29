package sidebar

import (
	"github.com/dkroshow/nano/internal/catalog"
	featuremail "github.com/dkroshow/nano/internal/features/mail"
	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/inspection"
	"github.com/dkroshow/nano/internal/threadindex"
)

// Thread combines one saved catalog row with its current visible overlays.
type Thread struct {
	threadindex.Thread
	HostedThreadID        string `json:"hostedThreadId,omitempty"`
	HostedStatus          string `json:"hostedStatus,omitempty"`
	InvokerHostedThreadID string `json:"invokerHostedThreadId,omitempty"`
	InvokerNativeThreadID string `json:"invokerNativeThreadId,omitempty"`
	InvokerProvider       string `json:"invokerProvider,omitempty"`
	SidebarPlacement      string `json:"sidebarPlacement,omitempty"`
	Transient             bool   `json:"transient,omitempty"`
	// Runtime is the volatile hosted-session overlay, stamped at response time
	// from the session registry (runtime.go) — never stored in SQLite, never
	// part of catalog_version. Absent = no nano session attached.
	Runtime *RuntimeInfo `json:"runtime,omitempty"`
	// Assignment is the current canonical hosted-thread assignment. It is a
	// read-time overlay beside hosted identity, never catalog storage.
	Assignment            *hostedstore.AssignmentView `json:"assignment,omitempty"`
	AssignmentUnavailable bool                        `json:"assignmentUnavailable,omitempty"`
	// CurrentResolution is the passive, current-environment provider setup. It
	// is attached only by the detail endpoint; list responses stay fast.
	CurrentResolution *inspection.CurrentResolution `json:"currentResolution,omitempty"`
	// Fleet is the read-time fleet-state classification (fleet_status.go): whose
	// court the ball is in and whether the loop is still alive. Computed per
	// response from the projected messages, never stored in SQLite and never
	// part of catalog_version. Absent when the catalog cannot classify the row.
	Fleet *catalog.FleetState `json:"fleet,omitempty"`
	// Court is the saved-file evidence Fleet was computed from, kept so the
	// fleet feed can re-evaluate the time bound without reading it again.
	Court catalog.FleetCourtMessage `json:"-"`
	// Starred is an operator pin overlay. It is stored in a sibling
	// SQLite file, never in the catalog, and stamped at response time.
	Starred bool `json:"starred"`
	// Mail is the durable-inbox count overlay: total, unread, and
	// read-but-undispositioned mail for the row's hosted thread. Read-time only,
	// from control.sqlite; absent without a hosted identity or without mail.
	Mail *featuremail.Counts `json:"mail,omitempty"`
}
