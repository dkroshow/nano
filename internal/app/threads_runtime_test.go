package app

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
	"testing"
	"time"

	state "github.com/dkroshow/nano/internal/app/statestore"
	turns "github.com/dkroshow/nano/internal/core/prompts"
	coreturns "github.com/dkroshow/nano/internal/core/turns"
	"github.com/dkroshow/nano/internal/edge/statewiring"
	statedb "github.com/dkroshow/nano/internal/state/db"
	"github.com/dkroshow/nano/internal/testsupport"
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
				err = control.EnsureHostedThread("rollback-owner", "codex", presentationShared, "", "", "")
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
				if _, err := resumed.control.View("rollback-owner"); !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("later schema reimported changed control facts: %v", err)
				}
			case "queued-turns.json":
				if rows := QueueSnapshot(resumed.queue); len(rows) != 0 {
					t.Fatalf("later schema reimported changed queue: %+v", rows)
				}
			case "send-attempts.json":
				if row, ok := resumed.attempts.Get("rollback"); ok {
					t.Fatalf("later schema reimported changed attempt: %+v", row)
				}
			case "turn-completed.sqlite":
				if row, err := fixtureLatestCompleted(resumed.completed, "rollback-owner"); !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("later schema reimported changed completion: %+v %v", row, err)
				}
			}
		})
	}
}

func TestThreadRuntimeMigrationUsesOneCanonicalDatabase(t *testing.T) {
	dir := t.TempDir()
	control, err := openControlMetadataStore(filepath.Join(dir, "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err := control.EnsureHostedThread("owner", "claude", presentationShared, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := control.ClaimNativeID("owner", "claude", "native", false); err != nil {
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
	if stores.queue.Database() != stores.control.CanonicalDatabase() || stores.attempts.Database() != stores.control.CanonicalDatabase() {
		t.Fatal("hosted writers use separate databases")
	}
	if got := QueueSnapshot(stores.queue); len(got) != 1 || got[0] != queued {
		t.Fatalf("runtime queue changed imported evidence: %+v", got)
	}
	if got, ok := stores.attempts.Get("action"); !ok || got != attempt {
		t.Fatalf("runtime journal changed imported evidence: %+v", got)
	}
	if owner := activeOwner(t, stores.control, "claude", "native"); owner != "owner" {
		t.Fatal("identity did not migrate")
	}
	if next, inserted, err := recordTestCompletedTurn(stores.completed, coreturns.Finish{HostedThreadID: "owner", Provider: "claude", TurnID: "next", FinalMessageID: "next-final", Outcome: "completed", At: time.Now(), Source: "adapter"}); err != nil || !inserted || next.ID != "18" {
		t.Fatalf("completion cursor lost: %+v %v %v", next, inserted, err)
	}

	var observers int
	if err := stores.control.CanonicalDatabase().QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name='retained_observer_fixture'`).Scan(&observers); err != nil || observers != 0 {
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
	if retained, ok := resumed.queue.Get("action"); !ok || retained.State != string(turns.Accepted) || retained.Text != "unconfirmed prompt" || retained.ProviderTurnID != "provider-turn" {
		t.Fatalf("restart changed accepted prompt evidence: %+v", retained)
	}
	if got, ok := resumed.attempts.Get("action"); !ok || got.Confirmed {
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
	// The completed adapter must use the same writer connection, not a second database handle.
	if err := stores.control.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := fixtureLatestCompleted(stores.completed, "owner"); err == nil {
		t.Fatal("completed adapter survived closure of the shared writer")
	}

}

func TestThreadRuntimeRetentionPreservesQueuedTextAndUncertainAttempts(t *testing.T) {
	db, path, err := testTurnDatabase(t, filepath.Join(t.TempDir(), "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	var queueRows []queuedTurnRecord
	for i := 0; i < testsupport.MaxQueuedTurns; i++ {
		queueRows = append(queueRows, queuedTurnRecord{ClientActionID: fmt.Sprint(i), Provider: "codex", NativeThreadID: "native", Text: "retained text", State: submissionAccepted})
	}
	if err := turns.SaveQueuedTurns(context.Background(), db, queueRows); err != nil {
		t.Fatal(err)
	}
	queue, err := newQueuedTurnStore(db, path, testPromptStore(t, db))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := QueueEnqueue(queue, queuedTurnRecord{ClientActionID: "overflow", Provider: "codex", NativeThreadID: "native", Text: "new text"}); err == nil {
		t.Fatal("queue capacity silently expired retained prompts")
	}
	var attempts []sendAttemptRecord
	for _, native := range []string{"native", "other"} {
		for i := 0; i < testsupport.MaxLegacySendAttempts+5; i++ {
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
	if len(JournalSnapshot(journal)) != len(attempts) {
		t.Fatal("opening journal silently pruned imported records")
	}
	if err := JournalPut(journal, sendAttemptRecord{ClientActionID: "new-queued", Provider: "codex", NativeThreadID: "native", Status: sendAttemptQueued}); err != nil {
		t.Fatal(err)
	}
	rows, err := turns.LoadSendAttempts(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2*testsupport.MaxLegacySendAttempts+6 {
		t.Fatalf("retention count = %d", len(rows))
	}
	for _, id := range []string{sendAttemptQueued, sendAttemptSending, sendAttemptTimeout, "unknown", "parked", "native-104", "other-104"} {
		if _, ok := journal.Get(id); !ok {
			t.Fatalf("retention lost %s", id)
		}
	}
	if _, ok := journal.Get("native-000"); ok {
		t.Fatal("old terminal history was not bounded")
	}
	retained, err := turns.LoadQueuedTurns(context.Background(), db)
	if err != nil || len(retained) != testsupport.MaxQueuedTurns {
		t.Fatal("journal retention changed queued text")
	}
}
