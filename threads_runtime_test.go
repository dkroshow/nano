package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	state "github.com/dkroshow/nano/internal/app/statestore"
	turns "github.com/dkroshow/nano/internal/core/prompts"
	coreturns "github.com/dkroshow/nano/internal/core/turns"
	"github.com/dkroshow/nano/internal/edge/statewiring"
	statedb "github.com/dkroshow/nano/internal/state/db"
	"github.com/dkroshow/nano/internal/threads"
)

func TestBootFailsClosedWhenTransactionalTurnStoreOpenFails(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var constructBody *ast.BlockStmt
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Name.Name == "construct" {
			constructBody = function.Body
			break
		}
	}
	if constructBody == nil {
		t.Fatal("construct function is unavailable")
	}
	var checked bool
	var inspectBlock func(*ast.BlockStmt)
	inspectBlock = func(block *ast.BlockStmt) {
		for index, statement := range block.List {
			assignment, ok := statement.(*ast.AssignStmt)
			if ok && callsFunction(assignment.Rhs, "openThreadRuntimeStoresAt") {
				if index+1 >= len(block.List) {
					t.Fatal("thread runtime open has no error gate")
				}
				gate, ok := block.List[index+1].(*ast.IfStmt)
				if !ok || len(gate.Body.List) == 0 {
					t.Fatal("thread runtime open error does not stop boot")
				}
				call, ok := gate.Body.List[0].(*ast.ExprStmt)
				if !ok || !isRequiredSchemaFatal(call.X) {
					t.Fatal("schema 22 open failure can continue to HTTP serving")
				}
				checked = true
			}
			ast.Inspect(statement, func(node ast.Node) bool {
				nested, ok := node.(*ast.BlockStmt)
				if ok && nested != block {
					inspectBlock(nested)
					return false
				}
				return true
			})
		}
	}
	inspectBlock(constructBody)
	if !checked {
		t.Fatal("main does not open the required transactional turn store")
	}
}

func callsFunction(expressions []ast.Expr, name string) bool {
	for _, expression := range expressions {
		call, ok := expression.(*ast.CallExpr)
		if !ok {
			continue
		}
		identifier, direct := call.Fun.(*ast.Ident)
		if direct && identifier.Name == name {
			return true
		}
	}
	return false
}

func isRequiredSchemaFatal(expression ast.Expr) bool {
	call, ok := expression.(*ast.CallExpr)
	if !ok || len(call.Args) == 0 {
		return false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	packageName, packageOK := selector.X.(*ast.Ident)
	message, messageOK := call.Args[0].(*ast.BasicLit)
	return packageOK && packageName.Name == "log" && (selector.Sel.Name == "Fatal" || selector.Sel.Name == "Fatalf") &&
		messageOK && strings.Contains(message.Value, "required schema %d turns store unavailable")
}

// Existing delivery fixtures name a queue/journal path. Both now resolve to
// one disposable SQLite file in that fixture directory, just as startup does.
var (
	transactionalTurnFixtureOnce sync.Once
	transactionalTurnFixtureData []byte
	transactionalTurnFixtureErr  error
)

func testTurnDatabase(t *testing.T, legacyPath string) (*sql.DB, string, error) {
	t.Helper()
	path := filepath.Join(filepath.Dir(legacyPath), "nano_threads.sqlite")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		data, err := transactionalTurnFixture()
		if err != nil {
			return nil, path, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, path, err
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			return nil, path, err
		}
	} else if err != nil {
		return nil, path, err
	}
	db, err := statedb.Open(context.Background(), path)
	if err != nil {
		return nil, path, err
	}
	t.Cleanup(func() { _ = db.Close() })
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return nil, path, err
	}
	if version != turns.TurnSchemaVersion {
		return nil, path, fmt.Errorf("schema %d is not current schema %d", version, turns.TurnSchemaVersion)
	}
	return db, path, nil
}

func transactionalTurnFixture() ([]byte, error) {
	transactionalTurnFixtureOnce.Do(func() {
		dir, err := os.MkdirTemp("", "nano-schema21-")
		if err != nil {
			transactionalTurnFixtureErr = err
			return
		}
		defer os.RemoveAll(dir)
		path := filepath.Join(dir, "source.sqlite")
		db, err := statedb.Open(context.Background(), path)
		if err != nil {
			transactionalTurnFixtureErr = err
			return
		}
		defer db.Close()
		if err := state.MigrateLegacySchema(db, statewiring.Migrations()); err != nil {
			transactionalTurnFixtureErr = err
			return
		}
		if _, err := turns.MigrateTurnRecords(context.Background(), db, turns.LegacyTurnPaths{}, filepath.Join(dir, "v19-backup")); err != nil {
			transactionalTurnFixtureErr = err
			return
		}
		if _, err := turns.MigrateTurnTransactions(context.Background(), db, filepath.Join(dir, "v20.sqlite")); err != nil {
			transactionalTurnFixtureErr = err
			return
		}
		if _, err := coreturns.MigrateCompletedTurnUsage(context.Background(), db, filepath.Join(dir, "v21.sqlite")); err != nil {
			transactionalTurnFixtureErr = err
			return
		}
		if _, err := turns.MigrateMemberRecovery(context.Background(), db, filepath.Join(dir, "v22.sqlite")); err != nil {
			transactionalTurnFixtureErr = err
			return
		}
		if _, err := turns.MigrateParkedDiscard(context.Background(), db, filepath.Join(dir, "v23.sqlite")); err != nil {
			transactionalTurnFixtureErr = err
			return
		}
		backup := filepath.Join(dir, "schema24.sqlite")
		if err := threads.Backup(context.Background(), db, backup); err != nil {
			transactionalTurnFixtureErr = err
			return
		}
		transactionalTurnFixtureData, transactionalTurnFixtureErr = os.ReadFile(backup)
	})
	return transactionalTurnFixtureData, transactionalTurnFixtureErr
}

func TestTurnDatabaseFixtureRefusesExistingLegacySchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nano_threads.sqlite")
	db, err := statedb.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.MigrateLegacySchema(db, statewiring.Migrations()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := testTurnDatabase(t, filepath.Join(dir, "queue.json")); err == nil || !strings.Contains(err.Error(), "schema 19 is not current schema 24") {
		t.Fatalf("existing legacy fixture was not refused: %v", err)
	}
}

func TestTurnDatabaseFixturesAreIsolated(t *testing.T) {
	first, _, err := testTurnDatabase(t, filepath.Join(t.TempDir(), "queue.json"))
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := testTurnDatabase(t, filepath.Join(t.TempDir(), "queue.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := turns.SaveQueuedTurns(context.Background(), first, []turns.QueuedTurn{{ClientActionID: "first", Provider: "codex", NativeThreadID: "native", Text: "retained"}}); err != nil {
		t.Fatal(err)
	}
	rows, err := turns.LoadQueuedTurns(context.Background(), second)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("second fixture contains first fixture data: %+v", rows)
	}
}

func TestThreadRuntimeKeepsCanonicalFactsAfterLegacyChanges(t *testing.T) {
	for _, name := range []string{"control.sqlite", "queued-turns.json", "send-attempts.json", "turn-completed.sqlite"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			controlPath := filepath.Join(dir, "control.sqlite")
			control, err := openControlMetadataStore(controlPath)
			if err != nil {
				t.Fatal(err)
			}
			defer control.Close()
			for _, file := range []string{"queued-turns.json", "send-attempts.json"} {
				if err := os.WriteFile(filepath.Join(dir, file), []byte("[]"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			completedPath := filepath.Join(dir, "turn-completed.sqlite")
			completed, err := openCompletedTurnStore(completedPath)
			if err != nil {
				t.Fatal(err)
			}
			defer completed.Close()
			stores, err := openThreadRuntimeStores(ctx, dir, completedPath, "first")
			if err != nil {
				t.Fatal(err)
			}
			if err := stores.control.Close(); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "control.sqlite":
				err = control.ensureHostedThread("rollback-owner", "codex", presentationShared, "", "", "")
			case "queued-turns.json":
				err = os.WriteFile(filepath.Join(dir, name), []byte(`[{"clientActionId":"rollback","provider":"codex","hostedThreadId":"rollback-owner","text":"retained prompt"}]`), 0600)
			case "send-attempts.json":
				err = os.WriteFile(filepath.Join(dir, name), []byte(`[{"clientActionId":"rollback","provider":"codex","status":"uncertain"}]`), 0600)
			case "turn-completed.sqlite":
				err = recordLegacyCompletedTurn(completed, "rollback-owner", "codex", "turn", "final", time.Now(), "provider")
			}
			if err != nil {
				t.Fatal(err)
			}
			resumed, err := openThreadRuntimeStores(ctx, dir, completedPath, "restart")
			if err != nil {
				t.Fatal(err)
			}
			defer resumed.control.Close()
			switch name {
			case "control.sqlite":
				if _, err := resumed.control.view("rollback-owner"); !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("later schema reimported changed control facts: %v", err)
				}
			case "queued-turns.json":
				if rows := resumed.queue.snapshot(); len(rows) != 0 {
					t.Fatalf("later schema reimported changed queue: %+v", rows)
				}
			case "send-attempts.json":
				if row, ok := resumed.attempts.get("rollback"); ok {
					t.Fatalf("later schema reimported changed attempt: %+v", row)
				}
			case "turn-completed.sqlite":
				if row, err := resumed.completed.latest("rollback-owner"); !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("later schema reimported changed completion: %+v %v", row, err)
				}
			}
		})
	}
}
func newTestQueuedTurnStore(t *testing.T, path string) (*queuedTurnStore, error) {
	t.Helper()
	db, path, err := testTurnDatabase(t, path)
	if err != nil {
		return nil, err
	}
	return newQueuedTurnStore(db, path, testPromptStore(t, db))
}
func newTestSendAttemptStore(t *testing.T, path, runID string) (*sendAttemptStore, error) {
	t.Helper()
	db, path, err := testTurnDatabase(t, path)
	if err != nil {
		return nil, err
	}
	return newSendAttemptStore(db, path, runID, testPromptStore(t, db))
}

func TestThreadRuntimeMigrationUsesOneCanonicalDatabase(t *testing.T) {
	dir := t.TempDir()
	control, err := openControlMetadataStore(filepath.Join(dir, "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err := control.ensureHostedThread("owner", "claude", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := control.claimNativeID("owner", "claude", "native", false); err != nil {
		t.Fatal(err)
	}
	if err := control.Close(); err != nil {
		t.Fatal(err)
	}
	queued := queuedTurnRecord{ClientActionID: "action", Provider: "claude", HostedThreadID: "owner", NativeThreadID: "native", Text: "unconfirmed prompt", TextHash: "hash", RequestHash: "hash", State: submissionAccepted, ProviderTurnID: "provider-turn", CreatedAt: "2026-09-09T00:00:00Z"}
	attempt := sendAttemptRecord{ClientActionID: "action", Provider: "claude", NativeThreadID: "native", TextHash: "hash", Status: sendAttemptAccepted, ProviderTurnID: "provider-turn", CreatedAt: queued.CreatedAt, UpdatedAt: queued.CreatedAt, RunID: "old"}
	for name, value := range map[string]any{"queued-turns.json": []queuedTurnRecord{queued}, "send-attempts.json": []sendAttemptRecord{attempt}} {
		body, _ := json.Marshal(value)
		if err := os.WriteFile(filepath.Join(dir, name), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	legacyCompleted := filepath.Join(dir, "turn-completed.sqlite")
	old, err := openCompletedTurnStore(legacyCompleted)
	if err != nil {
		t.Fatal(err)
	}
	if err := recordLegacyCompletedTurn(old, "owner", "claude", "finished", "final", time.Now(), "provider"); err != nil {
		t.Fatal(err)
	}
	if _, err := old.db.Exec(`UPDATE sqlite_sequence SET seq=17 WHERE name='completed_turns'; CREATE TABLE retained_observer_fixture(id TEXT PRIMARY KEY); INSERT INTO retained_observer_fixture VALUES('observer')`); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	stores, err := openThreadRuntimeStores(context.Background(), dir, legacyCompleted, "new")
	if err != nil {
		t.Fatal(err)
	}
	defer stores.control.Close()
	if stores.queue.db != stores.control.db || stores.attempts.db != stores.control.db || stores.completed.db != stores.control.db {
		t.Fatal("hosted writers use separate databases")
	}
	if got := stores.queue.snapshot(); len(got) != 1 || got[0] != queued {
		t.Fatalf("runtime queue changed imported evidence: %+v", got)
	}
	if got, ok := stores.attempts.get("action"); !ok || got != attempt {
		t.Fatalf("runtime journal changed imported evidence: %+v", got)
	}
	if owner := activeOwner(t, stores.control, "claude", "native"); owner != "owner" {
		t.Fatal("identity did not migrate")
	}
	if next, inserted, err := recordTestCompletedTurn(stores.completed, coreturns.Finish{HostedThreadID: "owner", Provider: "claude", TurnID: "next", FinalMessageID: "next-final", Outcome: "completed", At: time.Now(), Source: "adapter"}); err != nil || !inserted || next.ID != "18" {
		t.Fatalf("completion cursor lost: %+v %v %v", next, inserted, err)
	}
	var observers int
	if err := stores.control.db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name='retained_observer_fixture'`).Scan(&observers); err != nil || observers != 0 {
		t.Fatal("observer state migrated with completion facts")
	}
	if _, err := stores.turns.Cancel(context.Background(), "action"); err == nil {
		t.Fatal("accepted prompt was canceled")
	}
	if err := stores.control.Close(); err != nil {
		t.Fatal(err)
	}
	// Accepted prompt evidence remains permanent and cannot be reopened after cutover.
	resumed, err := openThreadRuntimeStores(context.Background(), dir, legacyCompleted, "restart")
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.control.Close()
	if retained, ok := resumed.queue.get("action"); !ok || retained.State != string(turns.Accepted) || retained.Text != "unconfirmed prompt" || retained.ProviderTurnID != "provider-turn" {
		t.Fatalf("restart changed accepted prompt evidence: %+v", retained)
	}
	if got, ok := resumed.attempts.get("action"); !ok || got.Confirmed {
		t.Fatal("restart invented confirmation")
	}
	old, err = openCompletedTurnStore(legacyCompleted)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	var legacyTurnID string
	if err := old.db.QueryRow(`SELECT turn_id FROM completed_turns WHERE target_hosted_thread_id='owner' ORDER BY id DESC LIMIT 1`).Scan(&legacyTurnID); err != nil || legacyTurnID != "finished" {
		t.Fatal("canonical writer modified legacy completion source")
	}
	if err := old.db.QueryRow(`SELECT count(*) FROM retained_observer_fixture`).Scan(&observers); err != nil || observers != 1 {
		t.Fatal("observer state changed")
	}
}

func TestThreadRuntimeRetentionPreservesQueuedTextAndUncertainAttempts(t *testing.T) {
	db, path, err := testTurnDatabase(t, filepath.Join(t.TempDir(), "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	var queueRows []queuedTurnRecord
	for i := 0; i < maxQueuedTurns; i++ {
		queueRows = append(queueRows, queuedTurnRecord{ClientActionID: fmt.Sprint(i), Provider: "codex", NativeThreadID: "native", Text: "retained text", State: submissionAccepted})
	}
	if err := turns.SaveQueuedTurns(context.Background(), db, queueRows); err != nil {
		t.Fatal(err)
	}
	queue, err := newQueuedTurnStore(db, path, testPromptStore(t, db))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := queue.enqueue(queuedTurnRecord{ClientActionID: "overflow", Provider: "codex", NativeThreadID: "native", Text: "new text"}); err == nil {
		t.Fatal("queue capacity silently expired retained prompts")
	}
	var attempts []sendAttemptRecord
	for _, native := range []string{"native", "other"} {
		for i := 0; i < maxSendAttempts+5; i++ {
			attempts = append(attempts, sendAttemptRecord{ClientActionID: fmt.Sprintf("%s-%03d", native, i), Provider: "codex", NativeThreadID: native, Status: sendAttemptFailed, UpdatedAt: time.Unix(int64(i), 0).UTC().Format(time.RFC3339Nano)})
		}
	}
	for _, status := range []string{sendAttemptQueued, sendAttemptSending, sendAttemptTimeout, "unknown", "parked"} {
		attempts = append(attempts, sendAttemptRecord{ClientActionID: status, Provider: "codex", NativeThreadID: "native", Status: status})
	}
	if err := turns.SaveSendAttempts(context.Background(), db, attempts); err != nil {
		t.Fatal(err)
	}
	journal, err := newSendAttemptStore(db, path, "run", testPromptStore(t, db))
	if err != nil {
		t.Fatal(err)
	}
	if len(journal.snapshot()) != len(attempts) {
		t.Fatal("opening journal silently pruned imported records")
	}
	if err := journal.put(sendAttemptRecord{ClientActionID: "new-queued", Provider: "codex", NativeThreadID: "native", Status: sendAttemptQueued}); err != nil {
		t.Fatal(err)
	}
	rows, err := turns.LoadSendAttempts(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2*maxSendAttempts+6 {
		t.Fatalf("retention count = %d", len(rows))
	}
	for _, id := range []string{sendAttemptQueued, sendAttemptSending, sendAttemptTimeout, "unknown", "parked", "native-104", "other-104"} {
		if _, ok := journal.get(id); !ok {
			t.Fatalf("retention lost %s", id)
		}
	}
	if _, ok := journal.get("native-000"); ok {
		t.Fatal("old terminal history was not bounded")
	}
	retained, err := turns.LoadQueuedTurns(context.Background(), db)
	if err != nil || len(retained) != maxQueuedTurns {
		t.Fatal("journal retention changed queued text")
	}
}
