package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	hostedvalues "github.com/dkroshow/nano/internal/hosted"
	"github.com/dkroshow/nano/internal/sessions"
)

var (
	errNativeClaimOwned = hostedvalues.ErrNativeClaimOwned
	errNativeClaimLive  = errors.New("native identity is live-attached on another hosted record")
)

func (s *server) transferNativeClaim(targetHostedID, provider, nativeID string) error {
	control := s.controlStore()
	if s == nil || control == nil {
		return errHostedControlUnavailable
	}
	owner, err := control.ActiveNativeOwner(provider, nativeID)
	if err != nil {
		return err
	}
	if owner != "" && owner != targetHostedID {
		if _, live := s.liveHostedSession(owner); live {
			return errNativeClaimLive
		}
	}
	return control.TransferNativeClaim(targetHostedID, provider, nativeID, sessions.NativeThreadIDProvisional(provider, nativeID))
}

// resolveResumeHostedIdentity is a read-only routing decision. An exact active
// claim still wins. A conversation with no Nano history may proceed only after
// prepareExternalResume verifies current provider-owned source evidence; the
// actual registration remains an explicit spawn-time transaction, never a
// preview or identity-resolution side effect.
func (s *server) resolveResumeHostedIdentity(req *spawnRequest) error {
	if req == nil || req.Mode != "resume" {
		return nil
	}
	nativeID := strings.TrimSpace(req.ThreadID)
	if nativeID == "" || req.Provider == "" {
		return nil
	}
	control := s.controlStore()
	if control == nil {
		return errHostedControlUnavailable
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
		return s.prepareExternalResume(context.Background(), req)
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

func (s *server) inheritResumePosture(hostedID, provider, nativeID string) error {
	if s == nil || s.controlStore() == nil || s.agentExecutions == nil || hostedID == "" || nativeID == "" {
		return nil
	}
	matches, err := s.agentExecutions.SessionsForNativeHistory(provider, nativeID)
	if err != nil {
		return err
	}
	for _, rec := range matches {
		if rec.ID == "" || rec.ID == hostedID {
			continue
		}
		if err := s.controlStore().InheritMissingDesiredPosture(hostedID, rec.ID); err != nil {
			return err
		}
	}
	return nil
}

func (s *server) repairCanonicalNativeIdentity(provider, nativeID, targetHostedID string) error {
	if s == nil || s.controlStore() == nil {
		return errHostedControlUnavailable
	}
	if err := s.transferNativeClaim(targetHostedID, provider, nativeID); err != nil {
		return err
	}
	return s.inheritResumePosture(targetHostedID, provider, nativeID)
}

func (s *server) controlStore() *controlMetadataStore {
	if s == nil {
		return nil
	}
	if s.controlMetadata != nil && s.controlMetadataErr == "" {
		return s.controlMetadata
	}
	if s.agentExecutions != nil {
		return s.agentExecutions.Control()
	}
	return nil
}
