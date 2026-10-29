package sessions

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/interact"
	threadvalues "github.com/dkroshow/nano/internal/threads"
)

const (
	permissionEventRequested = hostedstore.PermissionEventRequested
	permissionEventResolved  = hostedstore.PermissionEventResolved
)

func isPermissionInteraction(p interact.PendingInteraction) bool {
	if p.Method == grokPermissionMethod {
		return true
	}
	if p.Method == grokExitPlanMethod || p.Method == grokAskQuestionMethod {
		return false
	}
	switch p.Kind {
	case kindCommandApproval, kindFileChangeApproval, kindPermissions, kindToolPermission:
		return true
	default:
		return false
	}
}

func permissionRequestDetails(p interact.PendingInteraction) (toolName, summary, digest string) {
	var value any
	_ = json.Unmarshal(p.Params, &value)
	toolName = firstJSONText(value, "tool_name", "toolName")
	if toolName == "" {
		toolName = p.Kind
	}
	command := firstJSONText(value, "command", "cmd")
	if command == "" {
		summary = toolName
	} else {
		lines := strings.Count(command, "\n") + 1
		pipes := strings.Count(command, "|") - 2*strings.Count(command, "||")
		if pipes < 0 {
			pipes = 0
		}
		segments := strings.Count(command, "&&") + strings.Count(command, "||") +
			strings.Count(command, ";") + pipes + strings.Count(command, "\n") + 1
		summary = fmt.Sprintf("%s (%d bytes, %d lines, %d shell segments)", toolName, len(command), lines, segments)
	}
	hash := sha256.Sum256(p.Params)
	digest = hex.EncodeToString(hash[:])
	return toolName, summary, digest
}

func firstJSONText(value any, keys ...string) string {
	wanted := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		wanted[key] = struct{}{}
	}
	var walk func(any) string
	walk = func(current any) string {
		switch typed := current.(type) {
		case map[string]any:
			for _, key := range keys {
				if child, ok := typed[key]; ok {
					switch text := child.(type) {
					case string:
						if strings.TrimSpace(text) != "" {
							return strings.TrimSpace(text)
						}
					case []any:
						parts := make([]string, 0, len(text))
						for _, part := range text {
							if item, ok := part.(string); ok {
								parts = append(parts, item)
							}
						}
						if len(parts) > 0 {
							return strings.Join(parts, " ")
						}
					}
				}
			}
			for key, child := range typed {
				if _, skip := wanted[key]; skip {
					continue
				}
				if found := walk(child); found != "" {
					return found
				}
			}
		case []any:
			for _, child := range typed {
				if found := walk(child); found != "" {
					return found
				}
			}
		}
		return ""
	}
	return walk(value)
}

func (attachmentState *State) permissionModeForEvent() string {
	if attachmentState.identity.metadata != nil && attachmentState.identity.hostedThreadID != "" {
		if mode := attachmentState.identity.metadata.Posture(attachmentState.identity.hostedThreadID).PermissionMode; mode != "" {
			return mode
		}
	}
	return attachmentState.permissionMode
}

func (attachmentState *State) permissionEvent(p interact.PendingInteraction, eventKind, decision, source, outcome string) hostedstore.PermissionEvent {
	toolName, summary, digest := permissionRequestDetails(p)
	return hostedstore.PermissionEvent{
		HostedThreadID: attachmentState.identity.hostedThreadID, Provider: attachmentState.identity.provider, NativeThreadID: threadvalues.FirstNonEmpty(p.ThreadID, attachmentState.ThreadID()),
		NanoSessionID: attachmentState.identity.id, AttachmentGeneration: attachmentState.attachmentGeneration,
		ProviderRequestID: p.ID, TurnID: p.TurnID, ItemID: p.ItemID, Method: p.Method,
		InteractionKind: p.Kind, ToolName: toolName, EventKind: eventKind,
		PermissionMode: attachmentState.permissionModeForEvent(), Decision: decision, DecisionSource: source,
		Outcome: outcome, RequestSummary: summary, RequestSHA256: digest,
		ObservedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
}

func (delivery *Delivery) recordPermissionRequest(p interact.PendingInteraction) error {
	if !isPermissionInteraction(p) || delivery.identity.metadata == nil || delivery.identity.hostedThreadID == "" || delivery.identity.id == "" {
		return nil
	}
	return delivery.identity.metadata.AppendPermissionEvent(delivery.state.permissionEvent(p, permissionEventRequested, "", "", "requested"))
}

func (delivery *Delivery) recordPermissionResolution(p interact.PendingInteraction, decision, source, outcome string) error {
	if !isPermissionInteraction(p) || delivery.identity.metadata == nil || delivery.identity.hostedThreadID == "" || delivery.identity.id == "" {
		return nil
	}
	return delivery.identity.metadata.AppendPermissionEvent(delivery.state.permissionEvent(p, permissionEventResolved, decision, source, outcome))
}

func permissionDecisionSource(decision, outcome string) string {
	switch {
	case outcome == "sessionClosed":
		return "nano_session"
	case outcome == "resolvedElsewhere":
		return "provider"
	case outcome == "unreachable":
		return "transport"
	}
	switch decision {
	case decisionAccept, decisionAcceptForSession, decisionDecline, decisionCancel:
		return "operator"
	default:
		return "nano"
	}
}
