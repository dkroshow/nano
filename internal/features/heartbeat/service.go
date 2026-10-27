package heartbeat

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/dkroshow/nano/internal/interact"
)

const DefaultMaxWakesPerHour = 60

// Service is a wall-clock scheduler. It never reads a transcript and
// has no catch-up loop. Lifecycle reads and wake submissions cross only the
// public interact boundary.
type Service struct {
	Store         *Store
	Client        InteractClient
	Enabled       bool
	MaxWakesHour  int
	Logf          func(string, ...any)
	PrePing       PrePingRunner
	LastDedupeKey func(string) (string, error)
	usageCursors  map[string]string
	lastPruneDay  string
	cancel        context.CancelFunc
	done          sync.WaitGroup
	AttachmentMu  sync.RWMutex
}

type InteractClient interface {
	ReadAvailability(context.Context, string) (interact.LifecycleState, error)
	interact.PromptSubmitter
	interact.CompletedTurnSubscriber
}

func NewService(store *Store, client InteractClient, enabled bool, logf func(string, ...any)) *Service {
	if store == nil {
		return nil
	}
	if logf == nil {
		logf = log.Printf
	}
	return &Service{Store: store, Client: client, Enabled: enabled, MaxWakesHour: DefaultMaxWakesPerHour, Logf: logf,
		PrePing:       NewPrePingRunner("", ""),
		LastDedupeKey: store.LastDeliveredDedupeKey,
		usageCursors:  make(map[string]string)}
}

func (h *Service) Start(ctx context.Context) {
	if h == nil || !h.Enabled || h.cancel != nil {
		return
	}
	ctx, h.cancel = context.WithCancel(ctx)
	h.done.Add(1)
	go h.runTicks(ctx)
}

func (h *Service) Close() {
	if h == nil {
		return
	}
	if h.cancel != nil {
		h.cancel()
		h.done.Wait()
	}
}

// untilNextHeartbeatMinute aligns the scheduler to UTC minute boundaries so
// epochMinute (unix seconds / 60) advances by exactly one between ticks.
func untilNextHeartbeatMinute(now time.Time) time.Duration {
	now = now.UTC()
	next := now.Truncate(time.Minute).Add(time.Minute)
	return next.Sub(now)
}

func (h *Service) runTicks(ctx context.Context) {
	defer h.done.Done()
	timer := time.NewTimer(untilNextHeartbeatMinute(time.Now()))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case at := <-timer.C:
			h.takeMinute(ctx, at)
			timer.Reset(untilNextHeartbeatMinute(time.Now()))
		}
	}
}

// takeMinute evaluates every attachment due at this wall-clock minute. A
// process that was down for several minutes simply resumes at the current
// minute; there is no catch-up for minutes it missed.
func (h *Service) takeMinute(ctx context.Context, at time.Time) {
	if h == nil || !h.Enabled {
		return
	}
	epochMinute := at.UTC().Unix() / 60
	h.ReconcileUsage(ctx)
	day := at.UTC().Format("2006-01-02")
	if day != h.lastPruneDay {
		h.lastPruneDay = day
		if deleted, err := h.Store.PruneDecisionLog(at.AddDate(0, 0, -30)); err != nil {
			h.Logf("heartbeat decision retention: %v", err)
		} else if deleted > 0 {
			h.Logf("heartbeat decision retention removed %d old skip rows", deleted)
		}
	}
	due, err := h.Store.DueAttachments(epochMinute)
	if err != nil {
		h.Logf("heartbeat due attachments: %v", err)
		return
	}
	for _, attachment := range due {
		h.EvaluateDue(ctx, attachment, epochMinute, at)
	}
}

func heartbeatActionID(attachmentID string, epochMinute int64) string {
	return fmt.Sprintf("%s%s:%d", ActionPrefix, attachmentID, epochMinute)
}

// EvaluateDue claims once, reads lifecycle through interact, optionally runs
// the attachment's immutable template copy, and submits through interact.
func (h *Service) EvaluateDue(ctx context.Context, attachment Attachment, epochMinute int64, at time.Time) {
	h.AttachmentMu.RLock()
	defer h.AttachmentMu.RUnlock()
	current, err := h.Store.Attachment(attachment.ID)
	if err != nil || current.TargetHostedThreadID != attachment.TargetHostedThreadID {
		return
	}
	attachment = current
	decisionID, claimed, err := h.Store.ClaimWakeDecision(attachment.ID, epochMinute, at)
	if err != nil {
		h.Logf("heartbeat claim decision %s@%d: %v", attachment.ID, epochMinute, err)
		return
	}
	if !claimed {
		return
	}
	prePingFailed := attachment.PrePing != nil && attachment.PrePingFailureScriptID == attachment.PrePing.ScriptID && attachment.PrePingFailureRevision == attachment.PrePing.Revision
	if attachment.State == StatePaused && (attachment.PrePing == nil || attachment.PrePingSuspended || prePingFailed) {
		_ = h.Store.SettleWakeDecision(decisionID, GateManualPause, "", nil, OutcomeSkipped, "", "", at)
		return
	}
	lifecycle, err := h.Client.ReadAvailability(ctx, attachment.TargetHostedThreadID)
	if err != nil {
		_ = h.Store.SettleWakeDecision(decisionID, GateHardFacts, Unreachable, []string{"lifecycle_read_failed"}, OutcomeFailed, "", "lifecycle_read", at)
		return
	}
	if err := h.Store.ObserveHumanMessage(attachment.ID, lifecycle.LastHumanMessageAt, at); err != nil {
		h.Logf("heartbeat human activity %s: %v", attachment.ID, err)
	}
	if lifecycle.Availability != Available {
		if lifecycle.Availability == Unreachable && (lifecycle.AttachmentState == "exited" || lifecycle.AttachmentState == "deleted") {
			reason := "target is gone: " + lifecycle.AttachmentState
			if _, err := h.Store.PauseAttachment(attachment.ID, reason, Actor{Kind: "system", Name: "nano"}, at); err != nil {
				h.Logf("heartbeat self-pause %s: %v", attachment.ID, err)
			}
		}
		_ = h.Store.SettleWakeDecision(decisionID, GateHardFacts, lifecycle.Availability, lifecycle.AvailabilityReasons, OutcomeSkipped, "", "", at)
		return
	}
	prompt := attachment.Prompt
	var prePingAudit PrePingAudit
	if attachment.PrePing != nil {
		revision, err := h.Store.ScriptRevision(attachment.PrePing.ScriptID, attachment.PrePing.Revision)
		if err != nil {
			prePingAudit = PrePingAudit{ScriptID: attachment.PrePing.ScriptID, Revision: attachment.PrePing.Revision, Result: PrePingFail, Reason: "revision_unavailable"}
			h.failPrePing(ctx, attachment, decisionID, epochMinute, lifecycle, prePingAudit, at)
			return
		}
		lastDeliveredDedupeKey, err := h.LastDedupeKey(attachment.ID)
		if err != nil {
			prePingAudit = PrePingAudit{ScriptID: attachment.PrePing.ScriptID, Revision: attachment.PrePing.Revision, Result: PrePingFail, Reason: "dedupe_state_unavailable"}
			h.failPrePing(ctx, attachment, decisionID, epochMinute, lifecycle, prePingAudit, at)
			return
		}
		result, audit, err := h.PrePing(ctx, revision, PrePingInput{
			Version: 1, DueEpochMinute: epochMinute, TargetHostedThreadID: attachment.TargetHostedThreadID,
			Prompt: attachment.Prompt, Settings: attachment.PrePing.Settings, LastDeliveredDedupeKey: lastDeliveredDedupeKey,
		})
		prePingAudit = audit
		if err != nil || result.Decision == PrePingFail {
			h.failPrePing(ctx, attachment, decisionID, epochMinute, lifecycle, prePingAudit, at)
			return
		}
		if result.Decision == PrePingSkip {
			gate := GatePrePing
			if result.Reason == GateDedupeSkip {
				gate = GateDedupeSkip
			}
			_ = h.Store.SettleWakeDecision(decisionID, gate, lifecycle.Availability, lifecycle.AvailabilityReasons, OutcomeSkipped, "", "", at, prePingAudit)
			return
		}
		prompt = result.Prompt
	}
	if attachment.State == StatePaused {
		prompt = PausedWakePrompt(attachment.PauseReason, prompt)
	}
	if ok, err := h.Store.HasWakeCapacity(at.Add(-time.Hour), h.MaxWakesHour); err != nil || !ok {
		class := "global_cap"
		if err != nil {
			class = "global_cap_read"
		}
		_ = h.Store.SettleWakeDecision(decisionID, GateGlobalCap, lifecycle.Availability, lifecycle.AvailabilityReasons, OutcomeSkipped, "", class, at, prePingAudit)
		return
	}
	actionID := heartbeatActionID(attachment.ID, epochMinute)
	op, err := h.Client.SubmitPrompt(ctx, interact.SubmitPromptRequest{
		ThreadRef: attachment.TargetHostedThreadID, Prompt: prompt, ClientID: actionID,
		Origin: interact.SystemOrigin, RequireIdle: true,
	})
	if err != nil {
		if attachment.PrePing != nil {
			_ = h.Store.SettleWakeDecision(decisionID, GateDelivery, lifecycle.Availability, lifecycle.AvailabilityReasons, OutcomeFailed, "", "delivery", at, prePingAudit)
		} else {
			_ = h.Store.SettleWakeDecision(decisionID, GateDelivery, lifecycle.Availability, lifecycle.AvailabilityReasons, OutcomeFailed, "", "delivery", at)
		}
		return
	}
	if attachment.PrePing != nil {
		_ = h.Store.SettleWakeDecision(decisionID, GateDelivery, lifecycle.Availability, lifecycle.AvailabilityReasons, OutcomeDelivered, op.ID, "", at, prePingAudit)
	} else {
		_ = h.Store.SettleWakeDecision(decisionID, GateDelivery, lifecycle.Availability, lifecycle.AvailabilityReasons, OutcomeDelivered, op.ID, "", at)
	}
	h.Logf("heartbeat wake attachment=%s operation=%s usage=pending", attachment.ID, op.ID)
}

func (h *Service) failPrePing(ctx context.Context, attachment Attachment, decisionID, epochMinute int64, lifecycle interact.LifecycleState, audit PrePingAudit, at time.Time) {
	if audit.Reason == "" {
		audit.Reason = "processor_failure"
	}
	wasPaused := attachment.State == StatePaused
	_, latched, err := h.Store.LatchPrePingFailure(attachment.ID, attachment.Revision, audit.ScriptID, audit.Revision, audit.Reason, at)
	if err != nil {
		h.Logf("heartbeat pre-ping latch %s@%d: %v", attachment.ID, epochMinute, err)
		_ = h.Store.SettleWakeDecision(decisionID, GatePrePing, lifecycle.Availability, lifecycle.AvailabilityReasons, OutcomeFailed, "", "failure_latch", at, audit)
		return
	}
	if !latched {
		_ = h.Store.SettleWakeDecision(decisionID, GatePrePing, lifecycle.Availability, lifecycle.AvailabilityReasons, OutcomeSkipped, "", "configuration_changed", at, audit)
		return
	}
	audit.FailureLatched = true
	if wasPaused {
		_ = h.Store.SettleWakeDecision(decisionID, GatePrePing, lifecycle.Availability, lifecycle.AvailabilityReasons, OutcomeFailed, "", "pre_ping_"+audit.Reason, at, audit)
		return
	}
	diagnostic := fmt.Sprintf("Nano could not run this heartbeat's pre-ping script (%s revision %d; %s). Nano paused the heartbeat after this one diagnostic so it will not repeat the failure. Update the attachment or explicitly resume it after fixing the script.", audit.ScriptID, audit.Revision, audit.Reason)
	actionID := heartbeatActionID(attachment.ID, epochMinute)
	op, err := h.Client.SubmitPrompt(ctx, interact.SubmitPromptRequest{
		ThreadRef: attachment.TargetHostedThreadID, Prompt: diagnostic, ClientID: actionID,
		Origin: interact.SystemOrigin, RequireIdle: true,
	})
	if err != nil {
		h.Logf("heartbeat pre-ping diagnostic %s@%d: %v", attachment.ID, epochMinute, err)
		_ = h.Store.SettleWakeDecision(decisionID, GatePrePing, lifecycle.Availability, lifecycle.AvailabilityReasons, OutcomeFailed, "", "diagnostic_delivery", at, audit)
		return
	}
	_ = h.Store.SettleWakeDecision(decisionID, GatePrePing, lifecycle.Availability, lifecycle.AvailabilityReasons, OutcomeFailed, op.ID, "pre_ping_"+audit.Reason, at, audit)
}

func (h *Service) ReconcileUsage(ctx context.Context) {
	targets, err := h.Store.PendingUsageTargets()
	if err != nil {
		h.Logf("heartbeat usage targets: %v", err)
		return
	}
	for _, target := range targets {
		cursor := h.usageCursors[target]
		for {
			page, err := h.Client.SubscribeCompletedTurns(ctx, interact.PageRequest{ThreadRef: target, Cursor: cursor, Limit: 200})
			if err != nil {
				h.Logf("heartbeat usage feed %s: %v", target, err)
				break
			}
			for _, turn := range page.Items {
				if turn.OperationID == "" {
					continue
				}
				if err := h.Store.RecordWakeUsage(turn.OperationID, turn.Usage); err != nil {
					h.Logf("heartbeat usage operation=%s: %v", turn.OperationID, err)
				} else if turn.Usage == nil {
					h.Logf("heartbeat wake operation=%s usage=unavailable", turn.OperationID)
				} else {
					h.Logf("heartbeat wake operation=%s usage=input:%s cached-input:%s output:%s", turn.OperationID, tokenValue(turn.Usage.InputTokens), tokenValue(turn.Usage.CachedInputTokens), tokenValue(turn.Usage.OutputTokens))
				}
			}
			if page.NextCursor == "" || page.NextCursor == cursor || len(page.Items) == 0 {
				break
			}
			cursor = page.NextCursor
			h.usageCursors[target] = cursor
		}
	}
}

func tokenValue(value *int64) string {
	if value == nil {
		return "unavailable"
	}
	return strconv.FormatInt(*value, 10)
}
