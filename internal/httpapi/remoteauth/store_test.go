package remoteauth

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Login grants must survive restart without replacing the password or cookies;
// rejected edits must preserve the previous grant set.
func TestRemoteAllowedLoginsPersistWithoutChangingPasswordOrSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remote-auth.sqlite")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if err := store.setPassword("correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	token, _, err := store.createSession("test browser", "other@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.setAllowedLogins("owner@example.com\nsecond@example.com\nowner@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := store.setAllowedLogins("owner@\nsecond@example.com"); err == nil {
		t.Fatal("partial login accepted")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	logins, err := store.allowedLogins()
	if err != nil || len(logins) != 2 || logins[0] != "owner@example.com" || logins[1] != "second@example.com" {
		t.Fatalf("persisted grants = %v, %v", logins, err)
	}
	if valid, err := store.verifyPassword("correct horse battery staple"); err != nil || !valid {
		t.Fatal("password changed")
	}
	if _, valid, err := store.validateSession(token); err != nil || !valid {
		t.Fatal("session revoked")
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("database permissions = %v, %v", info, err)
	}
	if err := store.setAllowedLogins(""); err != nil {
		t.Fatal(err)
	}
	if allowed, err := store.loginAllowed("owner@example.com"); err != nil || allowed {
		t.Fatalf("cleared grant still allowed = %v, %v", allowed, err)
	}
}

func testRemoteAuthStore(t *testing.T) *Store {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "remote-auth.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestRemoteAuthPasswordHashAndRotation(t *testing.T) {
	store := testRemoteAuthStore(t)
	configured, err := store.configured()
	if err != nil || configured {
		t.Fatalf("fresh configured = %v, err %v", configured, err)
	}
	if err := store.setPassword("too short"); err == nil {
		t.Fatal("short password accepted")
	}
	if err := store.setPassword("correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	for password, want := range map[string]bool{
		"correct horse battery staple":  true,
		"correct horse battery staples": false,
	} {
		got, err := store.verifyPassword(password)
		if err != nil || got != want {
			t.Fatalf("verify %q = %v, err %v; want %v", password, got, err, want)
		}
	}
	var encoded string
	if err := store.db.QueryRow(`SELECT password_hash FROM remote_auth_config WHERE id=1`).Scan(&encoded); err != nil {
		t.Fatal(err)
	}
	if encoded == "correct horse battery staple" || len(encoded) < 40 {
		t.Fatalf("password stored unsafely: %q", encoded)
	}

	_, _, err = store.createSession("test browser", "owner@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.setPassword("another strong remote password"); err != nil {
		t.Fatal(err)
	}
	sessions, err := store.listSessions()
	if err != nil || len(sessions) != 0 {
		t.Fatalf("sessions after rotation = %#v, err %v", sessions, err)
	}
}

func TestRemoteAuthSessionPersistsRefreshesAndExpires(t *testing.T) {
	store := testRemoteAuthStore(t)
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	token, created, err := store.createSession("Mozilla/5.0 (iPhone)", "owner@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if created.browserLabel() != "iPhone" {
		t.Fatalf("browser label = %q", created.browserLabel())
	}
	got, valid, err := store.validateSession(token)
	if err != nil || !valid || got.ID != created.ID || got.RefreshCookie {
		t.Fatalf("initial validate = %#v, valid %v, err %v", got, valid, err)
	}

	now = now.Add(25 * time.Hour)
	got, valid, err = store.validateSession(token)
	if err != nil || !valid || !got.RefreshCookie {
		t.Fatalf("refresh validate = %#v, valid %v, err %v", got, valid, err)
	}
	if want := now.Add(remoteSessionIdle); !got.ExpiresAt.Equal(want) {
		t.Fatalf("refreshed expiry = %v, want %v", got.ExpiresAt, want)
	}

	now = created.AbsoluteExpiry.Add(time.Second)
	_, valid, err = store.validateSession(token)
	if err != nil || valid {
		t.Fatalf("absolute-expired valid = %v, err %v", valid, err)
	}
}
