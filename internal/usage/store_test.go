package usage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreVersionsSchemaAndSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.sqlite")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := first.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("schema version = %d, err=%v", version, err)
	}
	observed := time.Now().UTC().Add(-time.Hour)
	first.Observe(&ProviderSnapshot{Provider: "claude", ObservedAt: observed, Buckets: []Bucket{{Key: "weekly", UsedPercent: 70}}})
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	snapshot := second.Snapshot()
	if len(snapshot) != 1 || len(snapshot[0].Buckets) != 1 || snapshot[0].Buckets[0].UsedPercent != 70 {
		t.Fatalf("restored snapshot = %#v", snapshot)
	}
}

func TestStoreRejectsNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.sqlite")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version=99`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := Open(path); err == nil {
		t.Fatal("newer schema was accepted")
	}
}

func TestStoreAppendsOnlyOnChangeAndKeepsLastGoodReading(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "usage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	reading := func(percent float64) *ProviderSnapshot {
		return &ProviderSnapshot{Provider: "codex", ObservedAt: time.Now().UTC(), Buckets: []Bucket{{Key: "weekly", UsedPercent: percent}}}
	}
	store.Observe(reading(50))
	store.Observe(reading(50))
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM usage_limit_observations`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("unchanged rows = %d, err=%v", count, err)
	}
	store.Observe((&ProviderSnapshot{Provider: "codex"}).Fail("unavailable", context.DeadlineExceeded))
	snapshot := store.Snapshot()[0]
	if len(snapshot.Buckets) != 1 || snapshot.Error != "unavailable" {
		t.Fatalf("failed snapshot = %#v", snapshot)
	}
}
