package app

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func threadsDatabaseSnapshot(t *testing.T, db *sql.DB) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	out["version"] = []string{fmt.Sprint(version)}
	rows, err := db.Query(`SELECT type,name,tbl_name,sql FROM sqlite_master ORDER BY type,name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var kind, name, table string
		var statement sql.NullString
		if err := rows.Scan(&kind, &name, &table, &statement); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal([]any{kind, name, table, statement})
		if err != nil {
			t.Fatal(err)
		}
		out["schema"] = append(out["schema"], string(encoded))
		if kind == "table" {
			tables = append(tables, name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	for _, table := range tables {
		rows, err := db.Query(`SELECT * FROM "` + strings.ReplaceAll(table, `"`, `""`) + `"`)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		out[table] = []string{}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			cells := make([]any, len(values))
			for i, value := range values {
				cells[i] = []any{fmt.Sprintf("%T", value), value}
			}
			encoded, err := json.Marshal(cells)
			if err != nil {
				t.Fatal(err)
			}
			out[table] = append(out[table], string(encoded))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		sort.Strings(out[table])
	}
	return out
}
