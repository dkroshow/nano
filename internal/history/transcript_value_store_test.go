package history

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestTranscriptValuesKeepExactTypesScopesAndKeys(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	projection := identityStoreProjection(t, store, "values")
	ctx := context.Background()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	empty := ""
	values := []any{json.RawMessage(`1`), "1", json.RawMessage(`1.0`), json.RawMessage(`"\u0031"`),
		"local:" + strings.Repeat("a", 64), "local:" + strings.Repeat("a", 64) + "/receipt", "snapshot:" + strings.Repeat("a", 64),
		"11111111-1111-4111-8111-111111111111", "11111111-1111-4111-8111-11111111111A",
		model.Scope{Provider: model.ProviderClaude}, model.Scope{Provider: model.ProviderClaude, NativeThreadID: &empty},
		model.Scope{Provider: model.ProviderClaude, Source: "native", Incarnation: "1", TurnKey: "local:" + strings.Repeat("b", 64), ItemID: "local:" + strings.Repeat("c", 64)}}
	ids := make(map[int64]bool)
	for _, value := range values {
		id, err := internTranscriptValueTx(ctx, tx, projection, value)
		if err != nil || ids[id] {
			t.Fatalf("typed values or unknown/empty scope collapsed: %v", err)
		}
		ids[id] = true
		again, err := internTranscriptValueTx(ctx, tx, projection, value)
		if err != nil || again != id {
			t.Fatal("exact value did not reuse its key", err)
		}
		var restored, expected any
		raw, _ := json.Marshal(value)
		if err := json.Unmarshal(raw, &expected); err != nil {
			t.Fatal(err)
		}
		if err := loadTranscriptValueTx(ctx, tx, projection, id, &restored); err != nil || !reflect.DeepEqual(restored, expected) {
			t.Fatal("interning altered exact native value", err)
		}
		if err := loadTranscriptValueTx(ctx, tx, projection+1, id, &restored); err == nil {
			t.Fatal("another projection borrowed an interned value")
		}
	}
	// Exact keys are reversible; there is no digest lookup that can alias values.
	for _, key := range [][]byte{{2, 'J', '1'}, {1, 'L', 0}, {1, 'U', 1}, {1, 'S', 0, 0, 0}, {1, 'S', 255}} {
		if _, err := decodeTranscriptValueKeyTx(ctx, tx, projection, key); err == nil {
			t.Fatalf("invalid key accepted: %x", key)
		}
	}
	cycle := []byte{1, 'S'}
	cycle = binary.AppendUvarint(cycle, 9999)
	cycle = append(cycle, 0, 0)
	if _, err := tx.Exec(`INSERT INTO conversation_parse_values(id,projection_id,value_key) VALUES (9999,?,?)`, projection, cycle); err != nil {
		t.Fatal(err)
	}
	if _, err := decodeTranscriptValueKeyTx(ctx, tx, projection, cycle); err == nil {
		t.Fatal("cyclic scope reference accepted")
	}
}

func testTranscriptValueKey(t *testing.T, value string) []byte {
	t.Helper()
	raw, _ := json.Marshal(value)
	key, err := encodeTranscriptValueKeyTx(context.Background(), nil, 0, raw, false)
	if err != nil {
		t.Fatal(err)
	}
	return key
}
func testTranscriptValueString(t *testing.T, key []byte) string {
	t.Helper()
	raw, err := decodeTranscriptScalarKey(key)
	if err != nil {
		t.Fatal(err)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestTranscriptScalarKeyRoundTripPreservesExactJSON(t *testing.T) {
	for _, raw := range []string{`1`, `1.0`, `"1"`, `"\u0031"`, `null`, `false`, `"local:` + strings.Repeat("b", 64) + `/catalog"`, `"snapshot:` + strings.Repeat("0", 64) + `"`, `"11111111-1111-4111-8111-111111111111"`} {
		key, err := encodeTranscriptValueKeyTx(context.Background(), nil, 0, []byte(raw), false)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := decodeTranscriptScalarKey(key)
		if err != nil || !bytes.Equal(decoded, []byte(raw)) {
			t.Fatalf("exact JSON changed: want=%s got=%s err=%v", raw, decoded, err)
		}
	}
}

func TestTranscriptValueScopesShareContextWithoutLosingLocalIdentity(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	projection := identityStoreProjection(t, store, "scope-size")
	ctx := context.Background()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := 0; i < 100; i++ {
		scope := model.Scope{Kind: model.ScopeItem, Provider: model.ProviderClaude, Source: strings.Repeat("source/", 50), Incarnation: "generation", TurnKey: "local:" + strings.Repeat("f", 64), ItemID: fmt.Sprintf("local:%064x", i)}
		id, err := internTranscriptValueTx(ctx, tx, projection, scope)
		if err != nil {
			t.Fatal(err)
		}
		var restored model.Scope
		if err := loadTranscriptValueTx(ctx, tx, projection, id, &restored); err != nil || !reflect.DeepEqual(scope, restored) {
			t.Fatalf("scope identity changed at %d: %v", i, err)
		}
	}
	var stored int
	if err := tx.QueryRow(`SELECT sum(length(value_key)) FROM conversation_parse_values WHERE projection_id=?`, projection).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored > 15000 {
		t.Fatalf("repeated scope context copied into keys: %d bytes", stored)
	}
}
