package history

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/grok"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestParsedTranscriptGrokLateReplayKeepsContentAndCompletion(t *testing.T) {
	raw, err := os.ReadFile("../transcriptparse/grok/testdata/late-replay-after-eviction.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for _, wire := range []bool{false, true} {
		name := "file"
		if wire {
			name = "wire"
		}
		t.Run(name, func(t *testing.T) {
			store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			projection := identityStoreProjection(t, store, "grok-late")
			tx, err := store.db.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			var state grok.State
			for i, frame := range bytes.Split(bytes.TrimSpace(raw), []byte{'\n'}) {
				input := model.Input{Bytes: frame, Source: model.SourceRef{Label: "fixture", Incarnation: "1"}}
				var batch model.Batch
				if wire {
					state, batch, err = grok.ParseWire(state, input)
				} else {
					input.Bytes = append(append([]byte(nil), frame...), '\n')
					state, batch, err = grok.ParseFile(state, input)
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := writeParsedTranscriptBatchTx(context.Background(), tx, projection, "s1", batch); err != nil {
					t.Fatal(err)
				}
				if i == 3 {
					for _, item := range state.Items {
						if item.Scope.NativeTurnID != nil && *item.Scope.NativeTurnID == "p1" {
							t.Fatal("fixture did not evict the completed turn")
						}
					}
					checkpoint, err := json.Marshal(state)
					if err != nil {
						t.Fatal(err)
					}
					state = grok.State{}
					if err := json.Unmarshal(checkpoint, &state); err != nil {
						t.Fatal(err)
					}
				}
			}
			var text, status, final string
			var count int
			if err := tx.QueryRow(`SELECT COUNT(*),MIN(i.display_text),MIN(t.status),COALESCE(MIN(t.final_item_id),'') FROM conversation_items i JOIN conversation_turns t ON t.projection_id=i.projection_id AND t.turn_key=i.turn_key WHERE i.projection_id=? AND t.native_turn_id='p1'`, projection).Scan(&count, &text, &status, &final); err != nil {
				t.Fatal(err)
			}
			if count != 1 || text != "AA" || status != string(TurnCompleted) || final == "" {
				t.Fatalf("late replay changed settled view: rows=%d text=%q status=%s final=%t", count, text, status, final != "")
			}
			if err := tx.QueryRow(`SELECT COUNT(*) FROM conversation_relations WHERE projection_id=? AND relation_kind='replay_of'`, projection).Scan(&count); err != nil || count != 1 {
				t.Fatalf("replay relations=%d err=%v", count, err)
			}
			var saved []byte
			if err := tx.QueryRow(`SELECT parsed_json FROM conversation_turns WHERE projection_id=? AND native_turn_id='p1'`, projection).Scan(&saved); err != nil {
				t.Fatal(err)
			}
			turn, err := decodeParsedTurnTx(context.Background(), tx, projection, saved)
			if err != nil {
				t.Fatal(err)
			}
			if turn.StartSourceSeq != 1 || len(turn.Times) == 0 || len(turn.Completion) != 1 {
				t.Fatal("partial later turn discarded original source position, clock or completion evidence")
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
