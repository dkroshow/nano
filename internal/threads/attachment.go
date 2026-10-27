package threads

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// AttachmentBinding is the durable fence and revision of one transport attempt.
// Generation is positive and fits SQLite's signed integer range. The lifecycle
// coordinator can convert it to an unsigned epoch after this transaction commits.
type AttachmentBinding struct {
	Generation int64
	Revision   int64
}

// BeginTransportBinding advances the existing hosted-thread generation and its
// audit row atomically. It never creates a missing thread or resets a counter.
func BeginTransportBinding(ctx context.Context, db *sql.DB, hostedID string) (AttachmentBinding, error) {
	if db == nil || strings.TrimSpace(hostedID) == "" {
		return AttachmentBinding{}, errors.New("threads: database and hosted identity are required")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return AttachmentBinding{}, fmt.Errorf("threads: begin attachment: %w", err)
	}
	defer tx.Rollback()
	binding, err := BeginTransportBindingTx(ctx, tx, hostedID)
	if err != nil {
		return AttachmentBinding{}, err
	}
	if err := tx.Commit(); err != nil {
		return AttachmentBinding{}, fmt.Errorf("threads: commit attachment: %w", err)
	}
	return binding, nil
}

// BeginTransportBindingTx performs the same allocation inside an existing
// identity transaction. Native recovery must commit its claim change, generation
// and activation intent together. The caller owns commit/rollback and must not
// expose this binding or attach a provider before the transaction commits.
func BeginTransportBindingTx(ctx context.Context, tx *sql.Tx, hostedID string) (AttachmentBinding, error) {
	if tx == nil || strings.TrimSpace(hostedID) == "" {
		return AttachmentBinding{}, errors.New("threads: transaction and hosted identity are required")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var binding AttachmentBinding
	// Bound both counters before arithmetic: SQLite promotes overflowing
	// integers to REAL, which cannot supply a durable lifecycle epoch.
	err := tx.QueryRowContext(ctx, `UPDATE hosted_threads
		SET attachment_generation=attachment_generation+1,revision=revision+1,updated_at=?
		WHERE hosted_thread_id=? AND attachment_generation>=0 AND attachment_generation<?
		AND revision>=0 AND revision<? RETURNING attachment_generation,revision`,
		now, hostedID, int64(math.MaxInt64), int64(math.MaxInt64)).Scan(&binding.Generation, &binding.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return AttachmentBinding{}, fmt.Errorf("threads: cannot bind %q: missing thread or invalid/exhausted counter", hostedID)
	}
	if err != nil {
		return AttachmentBinding{}, fmt.Errorf("threads: advance attachment: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO hosted_attachment_generations(hosted_thread_id,generation,bound_at) VALUES(?,?,?)`, hostedID, binding.Generation, now); err != nil {
		return AttachmentBinding{}, fmt.Errorf("threads: record attachment: %w", err)
	}
	return binding, nil
}
