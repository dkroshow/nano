package hosted

import (
	"context"
	"errors"
	"fmt"
	"strings"

	hostedstore "github.com/dkroshow/nano/internal/hosted/store"
	"github.com/dkroshow/nano/internal/sessions"
)

// Native claim errors distinguish another live attachment from a stored owner.
var (
	ErrNativeClaimOwned = hostedstore.ErrNativeClaimOwned
	ErrNativeClaimLive  = errors.New("native identity is live-attached on another hosted record")
)

// TransferNativeClaim moves a durable native claim only when its current owner is not live.
func (s *AttachmentCoordinator) TransferNativeClaim(targetHostedID, provider, nativeID string) error {
	control := s.ControlStore()
	if s == nil || control == nil {
		return ErrHostedControlUnavailable
	}
	owner, err := control.ActiveNativeOwner(provider, nativeID)
	if err != nil {
		return err
	}
	if owner != "" && owner != targetHostedID {
		if _, live := s.Registry.LiveHostedSummary(owner, s.Executions.SessionNeedsAttention); live {
			return ErrNativeClaimLive
		}
	}
	return control.TransferNativeClaim(targetHostedID, provider, nativeID, sessions.NativeThreadIDProvisional(provider, nativeID))
}

// ResolveResumeHostedIdentity is a read-only routing decision. An exact active
// claim still wins. A conversation with no Nano history may proceed only after
// PrepareExternalResume verifies current provider-owned source evidence; the
// actual registration remains an explicit spawn-time transaction, never a
// preview or identity-resolution side effect.
func (s *AttachmentCoordinator) ResolveResumeHostedIdentity(req *SpawnRequest) error {
	if req == nil || req.Mode != "resume" {
		return nil
	}
	nativeID := strings.TrimSpace(req.ThreadID)
	if nativeID == "" || req.Provider == "" {
		return nil
	}
	control := s.ControlStore()
	if control == nil {
		return ErrHostedControlUnavailable
	}
	named := strings.TrimSpace(req.HostedThreadID)
	if err := control.NativeRecoveryResumeGuard(context.Background(), named, nativeID, sessions.ProviderLifecycleCapabilities(req.Provider).NativeRecovery); err != nil {
		return err
	}
	owner, err := control.ActiveNativeOwner(req.Provider, nativeID)
	if err != nil {
		return err
	}
	if owner == "" {
		// A provisional handle retires when its resolution is recorded, which
		// commits before its attachment record is re-keyed. A record left under
		// the handle still resumes, and resumeClaudeSession finishes the re-key,
		// but only while the recorded native is that thread's current one and
		// nobody else owns it.
		if sessions.NativeThreadIDProvisional(req.Provider, nativeID) {
			resolvedOwner, err := control.ResolvedHandleOwner(context.Background(), named, req.Provider, nativeID)
			if err != nil {
				return err
			}
			if resolvedOwner != "" {
				req.HostedThreadID = resolvedOwner
				return nil
			}
		}
		if named != "" {
			return fmt.Errorf("hosted thread %s has no current claim for native conversation %s", named, nativeID)
		}
		return s.PrepareExternalResume(context.Background(), req)
	}
	if named != "" && named != owner {
		knows, err := control.HostedKnowsNative(named, req.Provider, nativeID)
		if err != nil {
			return err
		}
		if !knows {
			return fmt.Errorf("hosted thread %s is not native conversation %s", named, nativeID)
		}
	}
	req.HostedThreadID = owner
	return nil
}

// InheritResumePosture fills missing desired settings from prior hosted owners.
func (s *AttachmentCoordinator) InheritResumePosture(hostedID, provider, nativeID string) error {
	if s == nil || s.ControlStore() == nil || s.Executions == nil || hostedID == "" || nativeID == "" {
		return nil
	}
	matches, err := s.Executions.SessionsForNativeHistory(provider, nativeID)
	if err != nil {
		return err
	}
	for _, rec := range matches {
		if rec.ID == "" || rec.ID == hostedID {
			continue
		}
		if err := s.ControlStore().InheritMissingDesiredPosture(hostedID, rec.ID); err != nil {
			return err
		}
	}
	return nil
}

// RepairCanonicalNativeIdentity repairs a claim and its missing resume settings.
func (s *AttachmentCoordinator) RepairCanonicalNativeIdentity(provider, nativeID, targetHostedID string) error {
	if s == nil || s.ControlStore() == nil {
		return ErrHostedControlUnavailable
	}
	if err := s.TransferNativeClaim(targetHostedID, provider, nativeID); err != nil {
		return err
	}
	return s.InheritResumePosture(targetHostedID, provider, nativeID)
}

// ControlStore returns the available canonical metadata owner.
func (s *AttachmentCoordinator) ControlStore() *hostedstore.MetadataStore {
	if s == nil {
		return nil
	}
	if s.Metadata != nil && s.MetadataError == "" {
		return s.Metadata
	}
	if s.Executions != nil {
		return s.Executions.Control()
	}
	return nil
}
