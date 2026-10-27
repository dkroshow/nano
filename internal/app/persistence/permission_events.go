package persistence

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	PermissionEventRequested = "requested"
	PermissionEventResolved  = "resolved"
)

type PermissionEvent struct {
	ID                   int64  `json:"id"`
	HostedThreadID       string `json:"hostedThreadId"`
	Provider             string `json:"provider"`
	NativeThreadID       string `json:"nativeThreadId,omitempty"`
	NanoSessionID        string `json:"nanoSessionId"`
	AttachmentGeneration int64  `json:"attachmentGeneration"`
	ProviderRequestID    string `json:"providerRequestId"`
	TurnID               string `json:"turnId,omitempty"`
	ItemID               string `json:"itemId,omitempty"`
	Method               string `json:"method"`
	InteractionKind      string `json:"interactionKind"`
	ToolName             string `json:"toolName,omitempty"`
	EventKind            string `json:"eventKind"`
	PermissionMode       string `json:"permissionMode,omitempty"`
	Decision             string `json:"decision,omitempty"`
	DecisionSource       string `json:"decisionSource,omitempty"`
	Outcome              string `json:"outcome,omitempty"`
	RequestSummary       string `json:"requestSummary"`
	RequestSHA256        string `json:"requestSha256"`
	ObservedAt           string `json:"observedAt"`
	SourceEventKey       string `json:"-"`
}

type PermissionEventPage struct {
	Events     []PermissionEvent `json:"events"`
	NextCursor int64             `json:"nextCursor,omitempty"`
}

func AppendPermissionEvent(db *sql.DB, event PermissionEvent) error {
	if db == nil {
		return errors.New("canonical metadata store unavailable")
	}
	if event.HostedThreadID == "" || event.Provider == "" || event.NanoSessionID == "" || event.ProviderRequestID == "" {
		return errors.New("permission event identity is incomplete")
	}
	if event.EventKind != PermissionEventRequested && event.EventKind != PermissionEventResolved {
		return fmt.Errorf("invalid permission event kind %q", event.EventKind)
	}
	if event.ObservedAt == "" {
		event.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if event.SourceEventKey == "" {
		digest := sha256.Sum256([]byte(strings.Join([]string{event.HostedThreadID, event.NanoSessionID, event.ProviderRequestID, event.EventKind}, "\x00")))
		event.SourceEventKey = "permission:" + hex.EncodeToString(digest[:])
	}
	nullable := func(value string) any {
		if value == "" {
			return nil
		}
		return value
	}
	_, err := db.Exec(`INSERT INTO hosted_permission_events(
		hosted_thread_id,provider,native_thread_id,nano_session_id,attachment_generation,
		provider_request_id,turn_id,item_id,method,interaction_kind,tool_name,event_kind,
		permission_mode,decision,decision_source,outcome,request_summary,request_sha256,
		observed_at,source_event_key)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(source_event_key) DO NOTHING`,
		event.HostedThreadID, event.Provider, nullable(event.NativeThreadID), event.NanoSessionID,
		event.AttachmentGeneration, event.ProviderRequestID, nullable(event.TurnID), nullable(event.ItemID),
		event.Method, event.InteractionKind, nullable(event.ToolName), event.EventKind,
		nullable(event.PermissionMode), nullable(event.Decision), nullable(event.DecisionSource),
		nullable(event.Outcome), event.RequestSummary, event.RequestSHA256, event.ObservedAt, event.SourceEventKey)
	return err
}

func PermissionEventsAfter(db *sql.DB, hostedID string, after int64, limit int) (PermissionEventPage, error) {
	if db == nil {
		return PermissionEventPage{}, errors.New("canonical metadata store unavailable")
	}
	rows, err := db.Query(`SELECT event_id,hosted_thread_id,provider,COALESCE(native_thread_id,''),nano_session_id,
		attachment_generation,provider_request_id,COALESCE(turn_id,''),COALESCE(item_id,''),method,
		interaction_kind,COALESCE(tool_name,''),event_kind,COALESCE(permission_mode,''),COALESCE(decision,''),
		COALESCE(decision_source,''),COALESCE(outcome,''),request_summary,request_sha256,observed_at
		FROM hosted_permission_events WHERE hosted_thread_id=? AND event_id>? ORDER BY event_id LIMIT ?`, hostedID, after, limit)
	if err != nil {
		return PermissionEventPage{}, err
	}
	defer rows.Close()
	page := PermissionEventPage{Events: []PermissionEvent{}}
	for rows.Next() {
		var event PermissionEvent
		if err := rows.Scan(&event.ID, &event.HostedThreadID, &event.Provider, &event.NativeThreadID,
			&event.NanoSessionID, &event.AttachmentGeneration, &event.ProviderRequestID, &event.TurnID,
			&event.ItemID, &event.Method, &event.InteractionKind, &event.ToolName, &event.EventKind,
			&event.PermissionMode, &event.Decision, &event.DecisionSource, &event.Outcome,
			&event.RequestSummary, &event.RequestSHA256, &event.ObservedAt); err != nil {
			return PermissionEventPage{}, err
		}
		page.Events = append(page.Events, event)
		page.NextCursor = event.ID
	}
	return page, rows.Err()
}
