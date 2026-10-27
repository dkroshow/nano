package subscriptions

import (
	"context"
	"database/sql"
	"log"
	"sync"
	"time"

	"github.com/dkroshow/nano/internal/interact"
)

const (
	ActionPrefix           = "nano:subscription:"
	completedTurnPageLimit = 200
)

func IsFeatureAction(actionID string) bool {
	return len(actionID) > len(ActionPrefix) && actionID[:len(ActionPrefix)] == ActionPrefix
}

type Target struct {
	ChildHostedID   string
	InvokerHostedID string
}

// retryDelay paces a sweep that could not finish. A newly finished turn of
// the same child also tries again at once.
const retryDelay = 10 * time.Second

// Dispatcher reads a child's public completed-turn feed when the server says
// the child finished a turn, and once for every child at start. It never
// consumes the server's private operation transition log.
//
// Each child's notices go out on one worker at a time, which keeps its cursor
// in order. A delivery can wait as long as its recipient takes to accept it
// (minutes for a thread replaying after a restart), so no lock is held across
// one: a slow recipient delays only the notices addressed to it.
type Dispatcher struct {
	DB      *sql.DB
	Client  Client
	Display func(context.Context, string) string
	Logf    func(string, ...any)
	// mu guards the maps below.
	mu       sync.Mutex
	retrying map[string]bool
	// running marks a child whose worker is sweeping; again asks that worker
	// for one more pass, for a turn that finished after its feed read.
	running map[string]bool
	again   map[string]bool
}

func NewDispatcher(db *sql.DB, client Client, display func(context.Context, string) string, logf func(string, ...any)) *Dispatcher {
	if db == nil || client == nil {
		return nil
	}
	if logf == nil {
		logf = log.Printf
	}
	return &Dispatcher{DB: db, Client: client, Display: display, Logf: logf,
		retrying: map[string]bool{}, running: map[string]bool{}, again: map[string]bool{}}
}

// Start delivers what finished while the server was down.
func (d *Dispatcher) Start() {
	if d != nil {
		go d.Sweep("")
	}
}

// Sweep delivers the unread finished turns of every subscription whose child
// or invoker is hostedThreadID, or of every subscription when it is empty. It
// returns once each target's worker is done, or has been asked to go again.
func (d *Dispatcher) Sweep(hostedThreadID string) {
	if d == nil || d.DB == nil || d.Client == nil {
		return
	}
	targets, err := Targets(d.DB, hostedThreadID)
	if err != nil {
		d.Logf("subscription targets %q: %v", hostedThreadID, err)
		d.mu.Lock()
		d.retryLater(hostedThreadID)
		d.mu.Unlock()
		return
	}
	var swept sync.WaitGroup
	for _, target := range targets {
		swept.Add(1)
		go func() {
			defer swept.Done()
			d.sweepChild(target)
		}()
	}
	swept.Wait()
}

// sweepChild runs the child's worker, or asks the running one to go again.
func (d *Dispatcher) sweepChild(target Target) {
	child := target.ChildHostedID
	d.mu.Lock()
	if d.running[child] {
		d.again[child] = true
		d.mu.Unlock()
		return
	}
	d.running[child] = true
	d.mu.Unlock()
	for {
		ok := d.SweepTarget(target)
		d.mu.Lock()
		if !ok {
			d.retryLater(child)
		}
		if !d.again[child] {
			delete(d.running, child)
			d.mu.Unlock()
			return
		}
		delete(d.again, child)
		d.mu.Unlock()
	}
}

// retryLater sweeps key again after the retry delay, with one retry pending
// per key. The caller holds d.mu.
func (d *Dispatcher) retryLater(key string) {
	if d.retrying[key] {
		return
	}
	d.retrying[key] = true
	time.AfterFunc(retryDelay, func() {
		d.mu.Lock()
		delete(d.retrying, key)
		d.mu.Unlock()
		d.Sweep(key)
	})
}

// SweepTarget reports false when it stopped on a failure a later attempt may
// clear.
func (d *Dispatcher) SweepTarget(target Target) bool {
	if target.ChildHostedID == target.InvokerHostedID {
		d.Logf("subscription skip worker=%s: invoker is self", target.ChildHostedID)
		return true
	}
	cursor, initialized, err := Cursor(d.DB, target.ChildHostedID)
	if err != nil {
		d.Logf("subscription cursor worker=%s: %v", target.ChildHostedID, err)
		return false
	}
	if !initialized {
		cursor, err = d.latestCursor(target.ChildHostedID)
		if err != nil {
			d.Logf("subscription initialize worker=%s: %v", target.ChildHostedID, err)
			return false
		}
		if err := AdvanceCursor(d.DB, target.ChildHostedID, cursor); err != nil {
			d.Logf("subscription initialize cursor worker=%s: %v", target.ChildHostedID, err)
			return false
		}
		return true
	}
	for {
		pageCursor := cursor
		page, err := d.Client.SubscribeCompletedTurns(context.Background(), interact.PageRequest{
			ThreadRef: target.ChildHostedID, Cursor: pageCursor, Limit: completedTurnPageLimit,
		})
		if err != nil {
			d.Logf("subscription read worker=%s: %v", target.ChildHostedID, err)
			return false
		}
		for _, turn := range page.Items {
			// Read the durable choice at delivery, rather than retaining the
			// setting captured before a slow recipient or another completion.
			var policy string
			var legacyPrompt bool
			if err := d.DB.QueryRow(`SELECT terminal_notification_policy,notify_invoker_on_terminal FROM hosted_threads WHERE hosted_thread_id=?`, target.ChildHostedID).Scan(&policy, &legacyPrompt); err != nil {
				d.Logf("subscription settings worker=%s: %v", target.ChildHostedID, err)
				return false
			}
			choices := FromStored(policy, legacyPrompt)
			if choices.Inbox == Send || choices.Prompt == Send {
				worker := target.ChildHostedID
				if d.Display != nil {
					worker = d.Display(context.Background(), target.ChildHostedID)
				}
				event := Event{
					ChildThreadRef: target.ChildHostedID, InvokerThreadRef: target.InvokerHostedID,
					OperationID: turn.OperationID, TurnID: turn.TurnID,
					WorkerDisplay: worker, Status: turn.Outcome,
				}
				if err := Deliver(context.Background(), d.Client, choices, event); err != nil {
					d.Logf("subscription deliver worker=%s turn=%s invoker=%s: %v", target.ChildHostedID, turn.TurnID, target.InvokerHostedID, err)
					if RecipientGone(d.DB, target.InvokerHostedID) {
						if err := AdvanceCursor(d.DB, target.ChildHostedID, turn.TurnID); err != nil {
							d.Logf("subscription advance permanent failure worker=%s turn=%s: %v", target.ChildHostedID, turn.TurnID, err)
							return false
						}
						cursor = turn.TurnID
						continue
					}
					return false
				}
			}
			if err := AdvanceCursor(d.DB, target.ChildHostedID, turn.TurnID); err != nil {
				d.Logf("subscription advance worker=%s turn=%s: %v", target.ChildHostedID, turn.TurnID, err)
				return false
			}
			cursor = turn.TurnID
		}
		if len(page.Items) == 0 || page.NextCursor == pageCursor || len(page.Items) < completedTurnPageLimit {
			return true
		}
	}
}

func (d *Dispatcher) latestCursor(child string) (string, error) {
	cursor := ""
	for {
		page, err := d.Client.SubscribeCompletedTurns(context.Background(), interact.PageRequest{ThreadRef: child, Cursor: cursor, Limit: completedTurnPageLimit})
		if err != nil {
			return "", err
		}
		if len(page.Items) == 0 || page.NextCursor == cursor {
			return cursor, nil
		}
		cursor = page.NextCursor
		if len(page.Items) < completedTurnPageLimit {
			return cursor, nil
		}
	}
}

func Cursor(db *sql.DB, child string) (string, bool, error) {
	var cursor string
	err := db.QueryRow(`SELECT cursor FROM subscription_completed_cursors WHERE hosted_thread_id=?`, child).Scan(&cursor)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return cursor, err == nil, err
}

func AdvanceCursor(db *sql.DB, child, cursor string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := db.Exec(`INSERT INTO subscription_completed_cursors(hosted_thread_id,cursor,initialized_at,updated_at)
		VALUES(?,?,?,?) ON CONFLICT(hosted_thread_id) DO UPDATE SET cursor=excluded.cursor,updated_at=excluded.updated_at`, child, cursor, now, now)
	return err
}

func InsertInitialCursor(tx *sql.Tx, child, now string) error {
	_, err := tx.Exec(`INSERT INTO subscription_completed_cursors(hosted_thread_id,cursor,initialized_at,updated_at) VALUES(?,?,?,?)`, child, "", now, now)
	return err
}

func RecipientGone(db *sql.DB, hostedID string) bool {
	var status string
	err := db.QueryRow(`SELECT e.status FROM hosted_threads h
		LEFT JOIN hosted_execution_sessions e ON e.hosted_thread_id=h.hosted_thread_id
		WHERE h.hosted_thread_id=?`, hostedID).Scan(&status)
	return err == sql.ErrNoRows || (err == nil && status == "exited")
}

// Targets lists the live subscriptions whose child or invoker is
// hostedThreadID, or every live subscription when it is empty.
func Targets(db *sql.DB, hostedThreadID string) ([]Target, error) {
	if db == nil {
		return nil, nil
	}
	rows, err := db.Query(`SELECT child.hosted_thread_id,child.invoker_hosted_id
		FROM hosted_threads child
		JOIN hosted_execution_sessions child_execution ON child_execution.hosted_thread_id=child.hosted_thread_id
		JOIN hosted_execution_sessions invoker_execution ON invoker_execution.hosted_thread_id=child.invoker_hosted_id
		WHERE child.invoker_hosted_id IS NOT NULL AND child.invoker_hosted_id<>''
			AND child_execution.status NOT IN ('detached','exited')
			AND invoker_execution.status NOT IN ('detached','exited')
			AND (?='' OR child.hosted_thread_id=? OR child.invoker_hosted_id=?)
		ORDER BY child.created_at,child.hosted_thread_id`, hostedThreadID, hostedThreadID, hostedThreadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var targets []Target
	for rows.Next() {
		var target Target
		if err := rows.Scan(&target.ChildHostedID, &target.InvokerHostedID); err != nil {
			return nil, err
		}
		// Disabled children still advance their cursor on completed turns,
		// so switching notices back on does not replay the quiet period.
		targets = append(targets, target)
	}
	return targets, rows.Err()
}
