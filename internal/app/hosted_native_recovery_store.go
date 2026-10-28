package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/dkroshow/nano/internal/sessions"
)

var errNativeRecoveryConflict = errors.New("native recovery conflict")

func (s *controlMetadataStore) nativeRecoveryResumeGuard(ctx context.Context, hostedID, provider, nativeID string) error {
	if !sessions.ProviderLifecycleCapabilities(provider).NativeRecovery {
		return nil
	}
	var count int
	err := s.ReadPool().QueryRowContext(ctx, `SELECT count(*) FROM hosted_native_recoveries WHERE
	 (old_native_id=? AND new_native_id<>'' AND state IN ('bound_pending_activation','activated')) OR
	 (state IN ('reserved','creating','created','bound_pending_activation','creation_unknown') AND
	 (hosted_thread_id=? OR old_native_id=? OR new_native_id=?))`, nativeID, hostedID, nativeID, nativeID).Scan(&count)
	if err != nil {
		return err
	}
	if count != 0 {
		return fmt.Errorf("%w: recovery-held or retired native identity", errNativeRecoveryConflict)
	}
	return nil
}

func (s *controlMetadataStore) retiredNativeRecoveryAlias(ctx context.Context, hostedID, nativeID string) (bool, error) {
	var count int
	err := s.ReadPool().QueryRowContext(ctx, `SELECT count(*) FROM hosted_native_recoveries r JOIN hosted_thread_native_ids n ON n.hosted_thread_id=r.hosted_thread_id AND n.native_thread_id=r.old_native_id AND n.provider='codex' WHERE r.hosted_thread_id=? AND r.old_native_id=? AND r.new_native_id<>'' AND r.state IN ('bound_pending_activation','activated') AND n.retired_at IS NOT NULL`, hostedID, nativeID).Scan(&count)
	return count > 0, err
}
