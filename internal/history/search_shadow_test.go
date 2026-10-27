package history

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// Search and reconciliation must ignore retained documents until their projection is active.
func TestSearchReadersAndCorpusExcludeInvisibleProjections(t *testing.T) {
	for _, state := range []string{"building", "abandoned", "superseded"} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			fixture := addSearchBackfillFixture(t, ctx, store)
			modes := []ConversationSearchMode{ConversationSearchLiteral, ConversationSearchTerms}
			buildSearchModes(t, ctx, store, modes, true, 10)
			if _, err := store.db.Exec(`UPDATE conversation_projections SET state=? WHERE projection_key=?`, state, fixture.MainProjectionKey); err != nil {
				t.Fatal(err)
			}
			var retained int
			if err := store.db.QueryRow(`SELECT COUNT(*) FROM conversation_search_documents`).Scan(&retained); err != nil {
				t.Fatal(err)
			}
			tx, err := store.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := maintainConversationSearchForThreadsTx(ctx, tx, []string{fixture.MainThreadID}); err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			assertCount(t, store.db, `SELECT COUNT(*) FROM conversation_search_documents`, retained)
			for _, mode := range modes {
				page, err := store.SearchConversationItems(ctx, ConversationSearchQuery{Query: "alpha", Mode: mode})
				if err != nil {
					t.Fatal(err)
				}
				if len(page.Items) != 0 {
					t.Errorf("%s exposed %d invisible rows", mode, len(page.Items))
				}
			}
			if err := reconcileConversationSearch(ctx, store.db, modes); err != nil {
				t.Errorf("invisible documents broke active corpus check: %v", err)
			}
			// Orphan rowids remain an alarm even while retained projections are invisible.
			for _, mode := range modes {
				if _, err := store.db.Exec(`INSERT INTO ` + searchFTSTable(mode) + `(rowid,text) VALUES(999999,'orphan text')`); err != nil {
					t.Fatal(err)
				}
				err := reconcileConversationSearch(ctx, store.db, []ConversationSearchMode{mode})
				if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("extra %s FTS rowid 999999", mode)) {
					t.Fatal("orphan FTS row was not reported", mode, err)
				}
			}
		})
	}
}
