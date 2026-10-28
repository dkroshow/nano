package hosted

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dkroshow/nano/internal/history"
	threadvalues "github.com/dkroshow/nano/internal/threads"
)

// Boundary: a result_not_saved operation reloaded from the control store, then
// the server's catalog commits for its native thread, with no session
// attached. Its exact answer, saved or indexed after the wait, must still
// complete it: found at startup, on a later commit, after a commit that came
// during a read, after a read the catalog cut short, or once a refused write
// succeeds. Another answer never does.
func TestCatalogCommitCompletesAnOperationWhoseAnswerOutlastedItsWait(t *testing.T) {
	type reply struct {
		item string
		err  error
	}
	for _, test := range []struct {
		name    string
		replies []reply // one per read, the last repeating
		commits int     // catalog commits after the startup read
		during  bool    // the first commit arrives while the startup read runs
		refused bool    // the store refuses finishing writes until the commits end
		status  string
		reads   int32
	}{
		{name: "indexed while Nano was down", replies: []reply{{item: "final-answer"}}, status: agentTurnCompleted, reads: 1},
		{name: "committed while detached", replies: []reply{{err: history.ErrSavedTranscriptUnavailable}, {item: "final-answer"}}, commits: 1, status: agentTurnCompleted, reads: 2},
		{name: "commit during a read", replies: []reply{{err: history.ErrSavedTranscriptUnavailable}, {item: "final-answer"}}, commits: 1, during: true, status: agentTurnCompleted, reads: 2},
		// A busy catalog cut the read short; no further commit comes.
		{name: "read cut short", replies: []reply{{err: history.ErrSavedTranscriptUnavailable}, {err: context.DeadlineExceeded}, {item: "final-answer"}}, commits: 1, status: agentTurnCompleted, reads: 3},
		{name: "refused write of a found answer", replies: []reply{{err: history.ErrSavedTranscriptUnavailable}, {item: "final-answer"}, {err: history.ErrSavedTranscriptUnavailable}}, commits: 1, refused: true, status: agentTurnCompleted, reads: 2},
		{name: "another answer saved", replies: []reply{{item: "other-answer"}}, commits: 2, status: agentTurnFailed, reads: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			first := newTestAgentExecutionStore(t, "result-run")
			hosted, err := first.ensureSession("hosted", "codex", presentationShared)
			if err != nil {
				t.Fatal(err)
			}
			if err := first.BindSession(hosted.ID, "session", "native-thread", "ready", presentationShared); err != nil {
				t.Fatal(err)
			}
			op, _, err := first.beginTurn(hosted.ID, "action", "prompt-hash")
			if err != nil {
				t.Fatal(err)
			}
			first.mu.Lock()
			rec := first.turns[op.ID]
			rec.Status, rec.ProviderTurnID, rec.FinalItemID = agentTurnFailed, "turn-1", "final-answer"
			rec.Error = &agentExecutionError{Source: "saved", Code: "result_not_saved", Message: "not saved"}
			rec.Revision++
			first.setTurnLocked(op.ID, &rec)
			err = first.persistAndNotifyLocked()
			first.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			if test.refused {
				if _, err := first.Control().CanonicalDatabase().Exec(`CREATE TRIGGER refuse_finish BEFORE UPDATE ON hosted_turn_operations
WHEN NEW.status = 'completed' BEGIN SELECT RAISE(ABORT, 'disk unavailable'); END`); err != nil {
					t.Fatal(err)
				}
			}
			store, err := newControlAgentExecutionStore(first.Control(), "next-run")
			if err != nil {
				t.Fatal(err)
			}
			var reads atomic.Int32
			inRead, release := make(chan struct{}), make(chan struct{})
			store.UseSavedResults(func(context.Context, agentTurnRecord) (string, string, error) {
				n := reads.Add(1)
				if test.during && n == 1 {
					close(inRead)
					<-release
				}
				r := test.replies[min(int(n), len(test.replies))-1]
				if r.err != nil {
					return "", "", r.err
				}
				return "saved answer", r.item, nil
			}, nil)
			commit := func() { store.SavedTranscriptApplied("codex", "native-thread") }
			if test.during {
				<-inRead
				commit()
				close(release)
			}
			for i := 0; i < test.commits; i++ {
				if test.during && i == 0 {
					continue
				}
				// Each commit after the previous read has ended, as the worker serializes them.
				waitForCatalogWithin(t, 3*time.Second, func() bool {
					store.mu.Lock()
					defer store.mu.Unlock()
					job := store.savedResultJobs[op.ID]
					return int(reads.Load()) > i && (job == nil || !job.reading)
				})
				commit()
			}
			if test.refused {
				// The found answer's write keeps retrying; no read replaces it.
				waitForCatalogWithin(t, 3*time.Second, func() bool { return reads.Load() == test.reads })
				time.Sleep(1500 * time.Millisecond)
				if _, err := first.Control().CanonicalDatabase().Exec(`DROP TRIGGER refuse_finish`); err != nil {
					t.Fatal(err)
				}
			}
			waitForCatalogWithin(t, 4*time.Second, func() bool {
				rec, _ := store.Turn(op.ID)
				store.mu.Lock()
				defer store.mu.Unlock()
				job := store.savedResultJobs[op.ID]
				return reads.Load() == test.reads && rec.Status == test.status && (job == nil || !job.reading)
			})
			result, _ := store.Turn(op.ID)
			if test.status == agentTurnCompleted && (result.FinalResponse != "saved answer" || result.FinalItemID != "final-answer" || result.Error != nil) ||
				test.status == agentTurnFailed && !threadvalues.AnswerNotSaved(result) {
				t.Fatalf("late result: %+v", result)
			}
		})
	}
}

// Boundary: one pending result's job, between its catalog reader and the
// control store, a second before its deadline. A slow catalog cannot extend
// the wait, and a refused commit retries the answer already found: it never
// reads again and loses that answer when a later read fails after expiry.
func TestOperationResultJobKeepsItsDeadlineAndItsAnswer(t *testing.T) {
	for _, test := range []struct {
		name                    string
		slow, recovers, refused bool
		status, answer          string
	}{
		{name: "slow catalog", slow: true, status: agentTurnFailed},
		// Cut short past the deadline, the failure is provisional: the job reads
		// again until the catalog answers.
		{name: "catalog answers after the wait", slow: true, recovers: true, status: agentTurnCompleted, answer: "saved answer"},
		{name: "refused commit", refused: true, status: agentTurnCompleted, answer: "saved answer"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store := newTestAgentExecutionStore(t, "result-run")
			hosted, err := store.ensureSession("hosted", "codex", presentationShared)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.BindSession(hosted.ID, "session", "native-thread", "ready", presentationShared); err != nil {
				t.Fatal(err)
			}
			op, _, err := store.beginTurn(hosted.ID, "action", "prompt-hash")
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(time.Second)
			store.mu.Lock()
			rec := store.turns[op.ID]
			rec.Status, rec.ProviderTurnID, rec.FinalItemID = agentTurnAwaitingSaved, "turn-1", "final-answer"
			rec.UpdatedAt = deadline.Add(-savedOperationResultWait).UTC().Format(time.RFC3339Nano)
			rec.Revision++
			store.setTurnLocked(op.ID, &rec)
			err = store.persistAndNotifyLocked()
			store.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			if test.refused {
				if _, err := store.Control().CanonicalDatabase().Exec(`CREATE TRIGGER refuse_finish BEFORE UPDATE ON hosted_turn_operations
WHEN NEW.status IN ('completed', 'failed') BEGIN SELECT RAISE(ABORT, 'disk unavailable'); END`); err != nil {
					t.Fatal(err)
				}
			}
			var reads atomic.Int32
			store.UseSavedResults(func(ctx context.Context, _ agentTurnRecord) (string, string, error) {
				recovered := test.recovers && time.Now().After(deadline.Add(savedResultLastRead))
				if !recovered && (reads.Add(1) > 1 || test.slow) {
					<-ctx.Done() // the catalog answers nothing in time
					return "", "", ctx.Err()
				}
				return "saved answer", "final-answer", nil
			}, nil)
			if test.refused {
				// Past the deadline, while every finishing write is refused.
				time.Sleep(time.Until(deadline) + savedResultLastRead)
				if rec, _ := store.Turn(op.ID); rec.Status != agentTurnAwaitingSaved {
					t.Fatalf("refused commit changed the operation: %+v", rec)
				}
				if _, err := store.Control().CanonicalDatabase().Exec(`DROP TRIGGER refuse_finish`); err != nil {
					t.Fatal(err)
				}
			}
			waitForCatalogWithin(t, time.Until(deadline)+savedResultLastRead+2*time.Second, func() bool { rec, _ := store.Turn(op.ID); return rec.Status == test.status })
			result, _ := store.Turn(op.ID)
			if result.Status != test.status || result.FinalResponse != test.answer || result.FinalItemID != "final-answer" {
				t.Fatalf("result: %+v", result)
			}
			if test.refused && reads.Load() != 1 {
				t.Fatalf("found answer was read again %d times", reads.Load()-1)
			}
		})
	}
}

func waitForCatalogWithin(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met before deadline")
}
