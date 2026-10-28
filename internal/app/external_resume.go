package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/dkroshow/nano/internal/history"
	hostedmodel "github.com/dkroshow/nano/internal/hosted"
	"github.com/dkroshow/nano/internal/threadindex"
)

type externalResumeSourceVerifier interface {
	VerifyExternalResumeSource(context.Context, string, string, string) (history.ThreadProjection, error)
}

// prepareExternalResume proves that a provider-native conversation exists and
// has never been registered in Nano. It is deliberately read-only so preview,
// link resolution, and opening a historical pane cannot create hosted state.
func (s *server) prepareExternalResume(ctx context.Context, req *spawnRequest) error {
	if s == nil || req == nil || req.Provider == "" || req.ThreadID == "" {
		return errors.New("provider and native conversation id are required")
	}
	if s.agentExecutions == nil || s.controlStore() == nil {
		return errHostedControlUnavailable
	}
	historyRecords, err := s.agentExecutions.SessionsForNativeHistory(req.Provider, req.ThreadID)
	if err != nil {
		return err
	}
	if len(historyRecords) != 0 {
		return fmt.Errorf("native conversation %s was previously hosted by Nano but has no current claim; refusing first registration", req.ThreadID)
	}
	if rec, ok := s.attachmentFor(req.Provider, req.ThreadID); ok && rec.HostedThreadID != "" {
		return fmt.Errorf("native conversation %s has a historical Nano attachment %s but no current claim", req.ThreadID, rec.HostedThreadID)
	}
	if s.catalog == nil {
		return fmt.Errorf("cannot verify native conversation %s: thread catalog unavailable", req.ThreadID)
	}
	page, err := s.catalog.ListThreads(ctx, threadindex.ListQuery{
		IncludeSubagents: true, IncludeGuardians: true, IncludeProgrammatic: true,
		Archived: string(history.ArchivedInclude), ThreadID: req.ThreadID, Limit: 1,
	})
	if err != nil {
		return fmt.Errorf("verify native conversation %s: %w", req.ThreadID, err)
	}
	if len(page.Items) == 0 {
		return fmt.Errorf("native conversation %s was not found in current provider history", req.ThreadID)
	}
	row := page.Items[0]
	if row.NativeID != req.ThreadID || row.Provider != req.Provider {
		return fmt.Errorf("native conversation %s belongs to provider %s, not %s", req.ThreadID, row.Provider, req.Provider)
	}
	detail, err := s.catalog.LoadThread(ctx, row.ID)
	if err != nil {
		return fmt.Errorf("load native conversation evidence for %s: %w", req.ThreadID, err)
	}
	if detail.NativeID != req.ThreadID || detail.Provider != req.Provider {
		return fmt.Errorf("native conversation %s provider identity changed during verification", req.ThreadID)
	}
	if detail.Source.Conflict {
		return fmt.Errorf("native conversation %s has conflicting provider source records", req.ThreadID)
	}
	if detail.Source.Status != string(history.SourcePresent) || detail.Sources == nil {
		return fmt.Errorf("native conversation %s has no current provider source", req.ThreadID)
	}
	verifiedSource := false
	for _, source := range *detail.Sources {
		if source.Status != string(history.SourcePresent) || source.IdentityEvidence != string(history.EvidenceNativeDurable) {
			continue
		}
		info, statErr := os.Lstat(source.Path)
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			continue
		}
		verifiedSource = true
		break
	}
	if !verifiedSource {
		return fmt.Errorf("native conversation %s lacks a present provider source with durable identity evidence", req.ThreadID)
	}
	if verifier, ok := s.catalog.(externalResumeSourceVerifier); ok {
		projection, err := verifier.VerifyExternalResumeSource(ctx, detail.ID, req.Provider, req.ThreadID)
		if err != nil {
			return fmt.Errorf("native conversation %s current provider source failed verification: %w", req.ThreadID, err)
		}
		detail.Metadata = threadindex.MapMetadata(projection.Metadata)
	}

	metadata := map[string]string{}
	for field, value := range map[string]threadindex.Value{
		"cwd":                                  detail.Metadata.CWD,
		hostedmodel.PostureFieldModel:          detail.Metadata.Model,
		hostedmodel.PostureFieldEffort:         detail.Metadata.Effort,
		hostedmodel.PostureFieldServiceTier:    detail.Metadata.Priority,
		hostedmodel.PostureFieldPermissionMode: detail.Metadata.Permission,
	} {
		if known := knownCatalogValue(value); known != "" {
			metadata[field] = known
		}
	}
	if metadata["cwd"] == "" {
		return fmt.Errorf("native conversation %s has no recorded working directory; Resume cannot safely choose one", req.ThreadID)
	}

	// Codex exposes a non-attaching provider read. The catalog proves durable
	// identity for every provider; thread/read additionally proves this daemon
	// can resolve the exact rollout before Nano commits registration.
	if req.Provider == "codex" {
		if s.codex == nil {
			return errors.New("cannot verify Codex resume availability: app-server unavailable")
		}
		readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		var result struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
		}
		if err := s.codex.Call(readCtx, "thread/read", map[string]any{"threadId": req.ThreadID, "includeTurns": false}, &result); err != nil {
			return fmt.Errorf("Codex conversation %s is not available to resume: %w", req.ThreadID, err)
		}
		if result.Thread.ID != req.ThreadID {
			return fmt.Errorf("Codex conversation verification returned %q for requested %q", result.Thread.ID, req.ThreadID)
		}
	}

	req.externalResume = true
	req.ExternalResumePosture = true
	req.externalResumeCatalog = detail.ID
	req.externalResumeMetadata = metadata
	req.Cwd = metadata["cwd"]
	req.Model = metadata[hostedmodel.PostureFieldModel]
	req.Effort = metadata[hostedmodel.PostureFieldEffort]
	req.ServiceTier = metadata[hostedmodel.PostureFieldServiceTier]
	req.PermissionMode = metadata[hostedmodel.PostureFieldPermissionMode]
	return nil
}
