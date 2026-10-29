// nano-threads inspects or copies the durable store. Every path is explicit;
// restore always creates a new path and never selects it for a running server.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/dkroshow/nano/internal/core/prompts"
	"github.com/dkroshow/nano/internal/edge/statewiring"
	"github.com/dkroshow/nano/internal/history"
	statedb "github.com/dkroshow/nano/internal/state/db"
	state "github.com/dkroshow/nano/internal/state/store"
	"github.com/dkroshow/nano/internal/threads"
	"modernc.org/sqlite"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, output io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: nano-threads <size|backup|restore|creation-name-preview|history-collect|prompts-cleanup> [options]")
	}
	operation := args[0]
	flags := flag.NewFlagSet("nano-threads "+operation, flag.ContinueOnError)
	flags.SetOutput(output)
	database := flags.String("database", "", "explicit store path (backup also accepts legacy control schema 19)")
	backup := flags.String("backup", "", "closed standalone backup to restore")
	destination := flags.String("out", "", "new output path; existing files are refused")
	requestPath := flags.String("request", "", "reviewed creation-name repair request JSON")
	historyDB := flags.String("db", "", "database path for history-collect or prompts-cleanup")
	dryRun := flags.Bool("dry-run", false, "report history collection or prompt cleanup without writing")
	maxMinutes := flags.Int("max-minutes", 20, "maximum history collection runtime")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if operation == "history-collect" {
		if *historyDB == "" || *database != "" || *backup != "" || *destination != "" || *requestPath != "" || *maxMinutes <= 0 {
			return errors.New("history-collect requires --db and a positive --max-minutes")
		}
		return runHistoryCollect(ctx, *historyDB, *dryRun, time.Duration(*maxMinutes)*time.Minute, output)
	}
	if operation == "prompts-cleanup" {
		if *historyDB == "" || *database != "" || *backup != "" || *destination != "" || *requestPath != "" || *maxMinutes != 20 {
			return errors.New("prompts-cleanup requires --db and accepts only --dry-run")
		}
		return runPromptsCleanup(ctx, *historyDB, *dryRun, output)
	}
	if *historyDB != "" || *dryRun || *maxMinutes != 20 {
		return errors.New("history collection flags require history-collect")
	}
	if operation == "restore" {
		if *backup == "" || *destination == "" || *database != "" || *requestPath != "" {
			return errors.New("restore requires --backup and --out")
		}
		if err := threads.Restore(ctx, *backup, *destination); err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(map[string]string{"operation": operation, "path": *destination})
	}
	if operation == "creation-name-preview" {
		if *database == "" || *requestPath == "" || *backup != "" || *destination != "" {
			return errors.New("creation-name-preview requires --database and --request")
		}
		file, err := os.Open(*requestPath)
		if err != nil {
			return err
		}
		var request threads.CreationNameRepairRequest
		decoder := json.NewDecoder(file)
		decoder.DisallowUnknownFields()
		decodeErr := decoder.Decode(&request)
		closeErr := file.Close()
		if err := errors.Join(decodeErr, closeErr); err != nil {
			return err
		}
		if request.Apply || request.ExpectedFingerprint != "" {
			return errors.New("creation-name-preview accepts only apply=false without expectedFingerprint")
		}
		db, err := state.OpenReadOnly(ctx, *database, statewiring.Migrations())
		if err != nil {
			return err
		}
		defer db.Close()
		receipt, err := threads.RepairCreationNames(ctx, db, request, "")
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(receipt)
	}
	if operation != "backup" && operation != "size" {
		return errors.New("unknown threads command")
	}
	if *database == "" || *backup != "" || *requestPath != "" || (operation == "backup" && *destination == "") || (operation == "size" && *destination != "") {
		return errors.New("size requires --database; backup requires --database and --out")
	}
	open := state.OpenReadOnly
	if operation == "backup" {
		open = state.OpenBackupSource
	}
	db, err := open(ctx, *database, statewiring.Migrations())
	if err != nil {
		return err
	}
	defer db.Close()
	if operation == "backup" {
		if err := threads.Backup(ctx, db, *destination); err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(map[string]string{"operation": operation, "path": *destination})
	}
	report, err := state.ReadSize(ctx, db, *database)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(report)
}

type promptCleanupCount struct {
	Accepted  int `json:"accepted"`
	Uncertain int `json:"uncertain"`
}

type promptCleanupRow struct {
	Provider       string `json:"provider"`
	ActionID       string `json:"clientActionId"`
	PreviousState  string `json:"previousState"`
	NewState       string `json:"newState"`
	ProviderTurnID string `json:"providerTurnId,omitempty"`
	CreatedAt      string `json:"createdAt"`
}

type promptCleanupReport struct {
	DryRun     bool                          `json:"dryRun"`
	Candidates int                           `json:"candidates"`
	Providers  map[string]promptCleanupCount `json:"providers"`
	Oldest     []promptCleanupRow            `json:"oldest"`
}

func runPromptsCleanup(ctx context.Context, path string, dryRun bool, output io.Writer) error {
	if _, err := os.Stat(path); err != nil {
		return err
	}
	var db *sql.DB
	var err error
	if dryRun {
		db, err = state.OpenReadOnly(ctx, path, statewiring.Migrations())
	} else {
		db, err = statedb.Open(ctx, path)
	}
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := prompts.OpenStore(ctx, db); err != nil {
		return err
	}
	// A confirmed prompt is settled by its saved transcript and is terminal;
	// cleanup never demotes it.
	rows, err := db.QueryContext(ctx, `SELECT provider,client_action_id,state,provider_turn_id,created_at FROM queued_turns WHERE state IN ('accepted','uncertain') ORDER BY created_at,position`)
	if err != nil {
		return err
	}
	var candidates []promptCleanupRow
	for rows.Next() {
		var row promptCleanupRow
		if err := rows.Scan(&row.Provider, &row.ActionID, &row.PreviousState, &row.ProviderTurnID, &row.CreatedAt); err != nil {
			rows.Close()
			return err
		}
		row.NewState = "uncertain"
		if row.ProviderTurnID != "" {
			row.NewState = "accepted"
		}
		candidates = append(candidates, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	report := promptCleanupReport{DryRun: dryRun, Candidates: len(candidates), Providers: map[string]promptCleanupCount{}}
	for _, row := range candidates {
		count := report.Providers[row.Provider]
		if row.NewState == "accepted" {
			count.Accepted++
		} else {
			count.Uncertain++
		}
		report.Providers[row.Provider] = count
		if len(report.Oldest) < 10 {
			report.Oldest = append(report.Oldest, row)
		}
	}
	if !dryRun {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for _, row := range candidates {
			journalState := "unknown"
			if row.NewState == "accepted" {
				journalState = "accepted"
			}
			// The row may have settled or expired since the scan; leave it.
			result, err := tx.ExecContext(ctx, `UPDATE queued_turns SET state=?,claim_token='',claim_epoch=0,revision=revision+1 WHERE client_action_id=? AND state IN ('accepted','uncertain')`, row.NewState, row.ActionID)
			if err != nil {
				return err
			}
			if changed, _ := result.RowsAffected(); changed != 1 {
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE send_attempts SET status=?,claim_token='',revision=revision+1 WHERE client_action_id=?`, journalState, row.ActionID); err != nil {
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return json.NewEncoder(output).Encode(report)
}

func runHistoryCollect(ctx context.Context, path string, dryRun bool, maxDuration time.Duration, output io.Writer) error {
	if dryRun {
		store, err := history.OpenReadOnly(path)
		if err != nil {
			return err
		}
		defer store.Close()
		type candidate struct {
			projectionID int64
			threadID     string
			items        int64
		}
		var largest []candidate
		candidates, items, err := store.SupersededProjectionSummary(ctx, 10, func(projectionID int64, threadID string, items int64) error {
			largest = append(largest, candidate{projectionID: projectionID, threadID: threadID, items: items})
			return nil
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(output, "history collector: dry-run candidates=%d items=%d\n", candidates, items)
		for rank, candidate := range largest {
			fmt.Fprintf(output, "history collector: largest rank=%d projection=%d thread=%s items=%d\n", rank+1, candidate.projectionID, candidate.threadID, candidate.items)
		}
		return nil
	}

	store, err := history.OpenMaintenance(path)
	if err != nil {
		return err
	}
	defer store.Close()
	collectionCtx, cancel := context.WithTimeout(ctx, maxDuration)
	defer cancel()
	stopped := "drained"
	var passes, collected int
	var items, aliases int64
	for pass := 1; ; pass++ {
		started := time.Now()
		batch, err := collectSupersededWithBusyRetry(collectionCtx, store, output)
		passes = pass
		collected += batch.Collected
		items += batch.Items
		fmt.Fprintf(output, "history collector: pass=%d collected=%d items=%d elapsed=%s\n", pass, batch.Collected, batch.Items, time.Since(started))
		for _, progress := range batch.Progress {
			fmt.Fprintf(output, "history collector: pass=%d projection=%d rows=%s\n", pass, progress.ProjectionID, progress.RowsSummary())
		}
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				stopped = "time_limit"
				break
			}
			return err
		}
		if !batch.Remaining {
			break
		}
	}
	for stopped == "drained" {
		removed, err := store.CollectDanglingItemAliasesBatch(collectionCtx, 5_000)
		aliases += removed
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				stopped = "time_limit"
				break
			}
			return err
		}
		if removed < 5_000 {
			break
		}
	}
	fmt.Fprintf(output, "history collector: complete stopped=%s passes=%d collected=%d items=%d aliases=%d\n", stopped, passes, collected, items, aliases)
	return nil
}

func collectSupersededWithBusyRetry(ctx context.Context, store *history.Store, output io.Writer) (history.SupersededProjectionBatch, error) {
	return collectSupersededWithRetry(ctx, output, func(ctx context.Context) (history.SupersededProjectionBatch, error) {
		return store.CollectSupersededProjectionsBatch(ctx, 64)
	}, isSQLiteBusy)
}

func collectSupersededWithRetry(ctx context.Context, output io.Writer, collect func(context.Context) (history.SupersededProjectionBatch, error), busy func(error) bool) (history.SupersededProjectionBatch, error) {
	backoff := 100 * time.Millisecond
	var total history.SupersededProjectionBatch
	var started time.Time
	for retry := 0; ; retry++ {
		batch, err := collect(ctx)
		mergeSupersededBatch(&total, batch)
		if err == nil || !busy(err) {
			return total, err
		}
		if started.IsZero() {
			started = time.Now()
		}
		if time.Since(started) >= 30*time.Second {
			return total, err
		}
		fmt.Fprintf(output, "history collector: retry=%d wait=%s reason=busy\n", retry+1, backoff)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return total, ctx.Err()
		case <-timer.C:
		}
		if backoff < 2*time.Second {
			backoff *= 2
			if backoff > 2*time.Second {
				backoff = 2 * time.Second
			}
		}
	}
}

func mergeSupersededBatch(total *history.SupersededProjectionBatch, batch history.SupersededProjectionBatch) {
	total.Collected += batch.Collected
	total.Items += batch.Items
	total.Remaining = batch.Remaining
	total.Progress = append(total.Progress, batch.Progress...)
}

func isSQLiteBusy(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && (sqliteErr.Code()&0xff == 5 || sqliteErr.Code()&0xff == 6)
}
