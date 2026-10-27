package grok

import (
	"strings"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// These are the fields projected by history's existing Grok summary adapter.
// Unknown fields remain explicit; a summary's newest model never revises turns.
func summaryMetadata(summary summaryDocument) model.Metadata {
	fields := model.Metadata{}
	for _, field := range []model.MetadataField{model.MetadataCWD, model.MetadataModel, model.MetadataEffort, model.MetadataPriority, model.MetadataPermission, model.MetadataHistoricalBranch, model.MetadataOrigin} {
		fields[field] = model.MetadataValue{State: model.MetadataUnknown}
	}
	for field, value := range map[model.MetadataField]string{
		model.MetadataNativeThreadID: summary.Info.ID,
		model.MetadataCWD:            summary.Info.CWD,
		model.MetadataModel:          summary.CurrentModelID,
		model.MetadataEffort:         summary.ReasoningEffort,
		model.MetadataPermission:     summary.SandboxProfile,
		model.MetadataTitle:          summary.GeneratedTitle,
		model.MetadataPreview:        catalogPreview(summary.SessionSummary),
	} {
		if value = strings.TrimSpace(value); value != "" {
			fields[field] = model.MetadataValue{State: model.MetadataKnown, Value: value}
		}
	}
	return fields
}

func catalogPreview(value string) string {
	value = strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
	runes := []rune(value)
	if len(runes) > 240 {
		return string(runes[:239]) + "…"
	}
	return value
}
