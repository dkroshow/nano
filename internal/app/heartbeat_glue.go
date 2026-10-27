package app

import (
	"database/sql"

	featureheartbeat "github.com/dkroshow/nano/internal/features/heartbeat"
	"github.com/dkroshow/nano/internal/interact"
	"github.com/dkroshow/nano/internal/threads"
)

const (
	defaultHeartbeatMaxWakesPerHour = featureheartbeat.DefaultMaxWakesPerHour
	heartbeatStateActive            = featureheartbeat.StateActive
	heartbeatStatePaused            = featureheartbeat.StatePaused
	heartbeatStateRemoved           = featureheartbeat.StateRemoved
	heartbeatGateManualPause        = featureheartbeat.GateManualPause
	heartbeatGateHardFacts          = featureheartbeat.GateHardFacts
	heartbeatGatePrePing            = featureheartbeat.GatePrePing
	heartbeatGateDedupeSkip         = featureheartbeat.GateDedupeSkip
	heartbeatGateGlobalCap          = featureheartbeat.GateGlobalCap
	heartbeatGateDelivery           = featureheartbeat.GateDelivery
	heartbeatOutcomeSkipped         = featureheartbeat.OutcomeSkipped
	heartbeatOutcomeDelivered       = featureheartbeat.OutcomeDelivered
	heartbeatOutcomeFailed          = featureheartbeat.OutcomeFailed
	heartbeatActionPrefix           = featureheartbeat.ActionPrefix
	heartbeatFooter                 = featureheartbeat.Footer
	heartbeatUsagePending           = featureheartbeat.UsagePending
	heartbeatUsageReported          = featureheartbeat.UsageReported
	heartbeatUsageUnavailable       = featureheartbeat.UsageUnavailable
	heartbeatPrePingWake            = featureheartbeat.PrePingWake
	heartbeatPrePingSkip            = featureheartbeat.PrePingSkip
	heartbeatPrePingFail            = featureheartbeat.PrePingFail
	heartbeatInboxTemplateID        = featureheartbeat.InboxTemplateID
	heartbeatTargetClosedReason     = featureheartbeat.TargetClosedReason
)

var (
	errHeartbeatAttachmentExists = featureheartbeat.ErrAttachmentExists
	errHeartbeatNotFound         = featureheartbeat.ErrNotFound
	errHeartbeatScriptNotFound   = featureheartbeat.ErrScriptNotFound
)

type heartbeatService = featureheartbeat.Service
type heartbeatStore = featureheartbeat.Store
type heartbeatActor = featureheartbeat.Actor
type heartbeatPrePingReference = featureheartbeat.PrePingReference
type heartbeatScript = featureheartbeat.Script
type heartbeatScriptRevision = featureheartbeat.ScriptRevision
type heartbeatScriptRevisionInput = featureheartbeat.ScriptRevisionInput
type heartbeatAttachment = featureheartbeat.Attachment
type heartbeatAttachmentInput = featureheartbeat.AttachmentInput
type heartbeatAttachmentPatch = featureheartbeat.AttachmentPatch
type heartbeatWakeDecision = featureheartbeat.WakeDecision
type heartbeatUsageSummary = featureheartbeat.UsageSummary
type heartbeatPrePingInput = featureheartbeat.PrePingInput
type heartbeatPrePingResult = featureheartbeat.PrePingResult
type heartbeatPrePingAudit = featureheartbeat.PrePingAudit
type heartbeatPrePingRunner = featureheartbeat.PrePingRunner
type heartbeatScriptTemplate = featureheartbeat.ScriptTemplate

func openHeartbeatStore(path string) (*heartbeatStore, error) {
	resolved, err := canonicalProspectivePath(path)
	if err != nil {
		return nil, err
	}
	return featureheartbeat.OpenStore(resolved, threads.Backup)
}

func newHeartbeatService(s *server, store *heartbeatStore, enabled bool, logf func(string, ...any)) *heartbeatService {
	return featureheartbeat.NewService(store, interact.NewHTTPClient(s.defaults.apiOrigin, nil), enabled, logf)
}

func newHeartbeatPrePingRunner(nodeBinary, apiOrigin string) heartbeatPrePingRunner {
	return featureheartbeat.NewPrePingRunner(nodeBinary, apiOrigin)
}

func loadHeartbeatScriptTemplates() ([]heartbeatScriptTemplate, error) {
	return featureheartbeat.LoadScriptTemplates()
}

func heartbeatScriptTemplateByID(id string) (heartbeatScriptTemplate, error) {
	return featureheartbeat.ScriptTemplateByID(id)
}

func normalizeJSONObject(raw []byte, max int) ([]byte, error) {
	return featureheartbeat.NormalizeJSONObject(raw, max)
}

func tableExists(db *sql.DB, table string) (bool, error) {
	return featureheartbeat.TableExists(db, table)
}

var _ featureheartbeat.InteractClient = (*interact.HTTPClient)(nil)
