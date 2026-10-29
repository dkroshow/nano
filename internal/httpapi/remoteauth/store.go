package remoteauth

// remote_auth_store.go owns Nano's small, non-rebuildable remote-auth store.
// It is deliberately separate from history.sqlite: catalog rebuilds must never
// erase a password or silently sign every remote browser out.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/mail"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dkroshow/nano/internal/configpaths"
	statedb "github.com/dkroshow/nano/internal/state/db"
	"golang.org/x/crypto/argon2"
	_ "modernc.org/sqlite"
)

const (
	remotePasswordMinRunes = 14
	remoteSessionIdle      = 30 * 24 * time.Hour
	remoteSessionAbsolute  = 90 * 24 * time.Hour
	remoteSessionRefresh   = 24 * time.Hour

	argonMemoryKiB   = 19 * 1024
	argonIterations  = 2
	argonParallelism = 1
	argonSaltBytes   = 16
	argonKeyBytes    = 32
)

type Store struct {
	db  *sql.DB
	now func() time.Time
}

type Session struct {
	ID             string
	CreatedAt      time.Time
	LastSeenAt     time.Time
	ExpiresAt      time.Time
	AbsoluteExpiry time.Time
	UserAgent      string
	TailscaleLogin string
	RefreshCookie  bool
}

func OpenStore(path string) (*Store, error) {
	path = strings.TrimSpace(path)
	absolute, err := configpaths.CanonicalProspective(path)
	if err != nil {
		return nil, fmt.Errorf("resolve remote auth database: %w", err)
	}
	db, err := statedb.Open(context.Background(), absolute)
	if err != nil {
		return nil, err
	}
	store := &Store{db: db, now: time.Now}
	if err := store.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS remote_auth_config (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			password_hash TEXT NOT NULL,
			updated_at INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS remote_auth_sessions (
			id TEXT PRIMARY KEY,
			token_hash BLOB NOT NULL UNIQUE,
			created_at INTEGER NOT NULL,
			last_seen_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL,
			absolute_expires_at INTEGER NOT NULL,
			user_agent TEXT NOT NULL,
			tailscale_login TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS remote_auth_allowed_logins (
			login TEXT PRIMARY KEY NOT NULL
		);
		CREATE INDEX IF NOT EXISTS remote_auth_sessions_expiry
			ON remote_auth_sessions(expires_at, absolute_expires_at);
	`)
	if err != nil {
		return fmt.Errorf("migrate remote auth database: %w", err)
	}
	return nil
}

func (s *Store) configured() (bool, error) {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM remote_auth_config WHERE id = 1`).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) allowedLogins() ([]string, error) {
	rows, err := s.db.Query(`SELECT login FROM remote_auth_allowed_logins ORDER BY login`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var logins []string
	for rows.Next() {
		var login string
		if err := rows.Scan(&login); err != nil {
			return nil, err
		}
		logins = append(logins, login)
	}
	return logins, rows.Err()
}

func (s *Store) loginAllowed(login string) (bool, error) {
	if login == "" {
		return false, nil
	}
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM remote_auth_allowed_logins WHERE login = ?`, login).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) setAllowedLogins(raw string) error {
	logins := strings.Fields(raw)
	for _, login := range logins {
		address, err := mail.ParseAddress(login)
		if err != nil || address.Address != login {
			return errors.New("enter full Tailscale login addresses, one per line")
		}
		for _, c := range login {
			if c < 33 || c > 126 {
				return errors.New("Tailscale login addresses must contain only ASCII characters")
			}
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM remote_auth_allowed_logins`); err != nil {
		return err
	}
	for _, login := range logins {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO remote_auth_allowed_logins(login) VALUES(?)`, login); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func validateRemotePassword(password string) error {
	if utf8.RuneCountInString(password) < remotePasswordMinRunes {
		return fmt.Errorf("password must contain at least %d characters", remotePasswordMinRunes)
	}
	if len(password) > 1024 {
		return errors.New("password is too long")
	}
	return nil
}

func (s *Store) setPassword(password string) error {
	if err := validateRemotePassword(password); err != nil {
		return err
	}
	encoded, err := hashRemotePassword(password)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO remote_auth_config(id,password_hash,updated_at)
		VALUES(1,?,?) ON CONFLICT(id) DO UPDATE SET password_hash=excluded.password_hash,updated_at=excluded.updated_at`,
		encoded, s.now().UTC().Unix()); err != nil {
		return err
	}
	// A password rotation is also an explicit "sign out everywhere" action.
	if _, err := tx.Exec(`DELETE FROM remote_auth_sessions`); err != nil {
		return err
	}
	return tx.Commit()
}

// SetPassword replaces the remote password and revokes every browser session.
func (s *Store) SetPassword(password string) error { return s.setPassword(password) }

func (s *Store) verifyPassword(password string) (bool, error) {
	var encoded string
	err := s.db.QueryRow(`SELECT password_hash FROM remote_auth_config WHERE id = 1`).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return verifyRemotePassword(password, encoded)
}

func hashRemotePassword(password string) (string, error) {
	salt := make([]byte, argonSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonIterations, argonMemoryKiB, argonParallelism, argonKeyBytes)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		argonMemoryKiB, argonIterations, argonParallelism,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

func verifyRemotePassword(password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" {
		return false, errors.New("unsupported password hash")
	}
	var memory uint32
	var iterations uint32
	var parallelism uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &parallelism); err != nil {
		return false, fmt.Errorf("parse password hash parameters: %w", err)
	}
	if memory < 8*1024 || memory > 256*1024 || iterations < 1 || iterations > 10 || parallelism < 1 || parallelism > 8 {
		return false, errors.New("password hash parameters outside safe bounds")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, errors.New("decode password salt")
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) < 16 || len(want) > 64 {
		return false, errors.New("decode password hash")
	}
	got := argon2.IDKey([]byte(password), salt, iterations, memory, parallelism, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

func randomRemoteToken(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func remoteTokenHash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func cleanAuthLabel(value string, max int) string {
	value = strings.TrimSpace(value)
	if len(value) <= max {
		return value
	}
	return value[:max]
}

func (s *Store) createSession(userAgent, tailscaleLogin string) (string, Session, error) {
	token, err := randomRemoteToken(32)
	if err != nil {
		return "", Session{}, fmt.Errorf("generate session token: %w", err)
	}
	id, err := randomRemoteToken(12)
	if err != nil {
		return "", Session{}, fmt.Errorf("generate session id: %w", err)
	}
	now := s.now().UTC()
	session := Session{
		ID: id, CreatedAt: now, LastSeenAt: now,
		ExpiresAt: now.Add(remoteSessionIdle), AbsoluteExpiry: now.Add(remoteSessionAbsolute),
		UserAgent: cleanAuthLabel(userAgent, 300), TailscaleLogin: cleanAuthLabel(tailscaleLogin, 200),
	}
	_, err = s.db.Exec(`INSERT INTO remote_auth_sessions
		(id,token_hash,created_at,last_seen_at,expires_at,absolute_expires_at,user_agent,tailscale_login)
		VALUES(?,?,?,?,?,?,?,?)`, session.ID, remoteTokenHash(token), session.CreatedAt.Unix(),
		session.LastSeenAt.Unix(), session.ExpiresAt.Unix(), session.AbsoluteExpiry.Unix(),
		session.UserAgent, session.TailscaleLogin)
	if err != nil {
		return "", Session{}, err
	}
	return token, session, nil
}

func scanRemoteSession(scanner interface{ Scan(...any) error }) (Session, error) {
	var session Session
	var created, seen, expires, absolute int64
	err := scanner.Scan(&session.ID, &created, &seen, &expires, &absolute, &session.UserAgent, &session.TailscaleLogin)
	if err != nil {
		return Session{}, err
	}
	session.CreatedAt = time.Unix(created, 0).UTC()
	session.LastSeenAt = time.Unix(seen, 0).UTC()
	session.ExpiresAt = time.Unix(expires, 0).UTC()
	session.AbsoluteExpiry = time.Unix(absolute, 0).UTC()
	return session, nil
}

func (s *Store) validateSession(token string) (Session, bool, error) {
	if token == "" || len(token) > 256 {
		return Session{}, false, nil
	}
	session, err := scanRemoteSession(s.db.QueryRow(`SELECT id,created_at,last_seen_at,expires_at,absolute_expires_at,user_agent,tailscale_login
		FROM remote_auth_sessions WHERE token_hash = ?`, remoteTokenHash(token)))
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, err
	}
	now := s.now().UTC()
	if !now.Before(session.ExpiresAt) || !now.Before(session.AbsoluteExpiry) {
		_, _ = s.db.Exec(`DELETE FROM remote_auth_sessions WHERE id = ?`, session.ID)
		return Session{}, false, nil
	}
	if now.Sub(session.LastSeenAt) >= remoteSessionRefresh {
		next := now.Add(remoteSessionIdle)
		if next.After(session.AbsoluteExpiry) {
			next = session.AbsoluteExpiry
		}
		if _, err := s.db.Exec(`UPDATE remote_auth_sessions SET last_seen_at=?,expires_at=? WHERE id=?`, now.Unix(), next.Unix(), session.ID); err != nil {
			return Session{}, false, err
		}
		session.LastSeenAt = now
		session.ExpiresAt = next
		session.RefreshCookie = true
	}
	return session, true, nil
}

func (s *Store) listSessions() ([]Session, error) {
	now := s.now().UTC().Unix()
	if _, err := s.db.Exec(`DELETE FROM remote_auth_sessions WHERE expires_at <= ? OR absolute_expires_at <= ?`, now, now); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT id,created_at,last_seen_at,expires_at,absolute_expires_at,user_agent,tailscale_login
		FROM remote_auth_sessions ORDER BY last_seen_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []Session
	for rows.Next() {
		session, err := scanRemoteSession(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

func (s *Store) revokeSession(id string) error {
	if len(id) < 8 || len(id) > 64 {
		return errors.New("invalid session id")
	}
	_, err := s.db.Exec(`DELETE FROM remote_auth_sessions WHERE id = ?`, id)
	return err
}

func (s *Store) revokeAllSessions() error {
	_, err := s.db.Exec(`DELETE FROM remote_auth_sessions`)
	return err
}

func (s Session) browserLabel() string {
	ua := s.UserAgent
	for _, candidate := range []struct{ contains, label string }{
		{"iPhone", "iPhone"}, {"iPad", "iPad"}, {"Macintosh", "Mac browser"},
		{"Android", "Android browser"}, {"Windows", "Windows browser"}, {"Linux", "Linux browser"},
	} {
		if strings.Contains(ua, candidate.contains) {
			return candidate.label
		}
	}
	if ua == "" {
		return "Unknown browser"
	}
	return "Browser " + strconv.Quote(cleanAuthLabel(ua, 42))
}
