package main

import (
	"io"
	"path/filepath"
	"testing"
)

func TestParseConfigResolvesDefaultDataPaths(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	config, err := parseConfigWith(nil, func() (string, error) { return home, nil }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(home, ".nanod")
	want := ConfigPaths{
		DataDir: base, HistoryDB: filepath.Join(base, "history.sqlite"), ThreadNames: filepath.Join(base, "thread-names.json"),
		ThreadsDB: filepath.Join(base, "nano_threads.sqlite"), LegacyControlDB: filepath.Join(base, "control.sqlite"),
		LegacyQueuedTurns: filepath.Join(base, "queued-turns.json"), LegacySendAttempts: filepath.Join(base, "send-attempts.json"),
		ThreadsBackups: filepath.Join(base, "threads-backups"), Attachments: filepath.Join(base, "attachments.json"),
		HeartbeatDB: filepath.Join(base, "heartbeat.sqlite"), MailDB: filepath.Join(base, "mail.sqlite"),
		CompletedTurnsDB: filepath.Join(base, "turn-completed.sqlite"), ThreadStarsDB: filepath.Join(base, "thread-stars.sqlite"), ThreadPinsDB: filepath.Join(base, "thread-pins.sqlite"),
		FeedbackDB: filepath.Join(base, "feedback.sqlite"),
		UsageDB:    filepath.Join(base, "usage-limits.sqlite"), RemoteAuthDB: filepath.Join(base, "remote-auth.sqlite"),
		MaintenanceBackups: filepath.Join(base, "maintenance-backups"), ClaudeMaintenanceReceipt: filepath.Join(base, "claude-submission-maintenance.json"),
		ServerLog: filepath.Join(base, "logs", "server.log"), WireSpoolDir: filepath.Join(base, "wire"),
	}
	if config.Paths != want {
		t.Fatalf("paths = %#v, want %#v", config.Paths, want)
	}
}

func TestParseConfigDataDirIsolatesEveryWriteTarget(t *testing.T) {
	base := filepath.Join(t.TempDir(), "candidate")
	config, err := parseConfigWith([]string{"-data-dir", base}, func() (string, error) { return "/unused", nil }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"history": config.Paths.HistoryDB, "names": config.Paths.ThreadNames, "threads": config.Paths.ThreadsDB,
		"legacy control": config.Paths.LegacyControlDB, "legacy queue": config.Paths.LegacyQueuedTurns,
		"legacy attempts": config.Paths.LegacySendAttempts, "thread backups": config.Paths.ThreadsBackups,
		"attachments": config.Paths.Attachments, "heartbeat": config.Paths.HeartbeatDB, "mail": config.Paths.MailDB,
		"completed turns": config.Paths.CompletedTurnsDB, "stars": config.Paths.ThreadStarsDB, "pins": config.Paths.ThreadPinsDB, "feedback": config.Paths.FeedbackDB, "usage": config.Paths.UsageDB,
		"remote auth": config.Paths.RemoteAuthDB, "maintenance backups": config.Paths.MaintenanceBackups,
		"maintenance receipt": config.Paths.ClaudeMaintenanceReceipt, "server log": config.Paths.ServerLog,
		"wire spool": config.Paths.WireSpoolDir, "shim spool": config.ClaudeShimSpoolDir,
	} {
		relative, err := filepath.Rel(base, path)
		if err != nil || relative == ".." || filepath.IsAbs(relative) && relative != "." || len(relative) >= 3 && relative[:3] == ".."+string(filepath.Separator) {
			t.Errorf("%s path %q is outside %q", name, path, base)
		}
	}
}

func TestParseConfigKeepsHistorySiblingCompatibility(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	history := filepath.Join(t.TempDir(), "catalog", "custom.sqlite")
	config, err := parseConfigWith([]string{"-history-db", history}, func() (string, error) { return home, nil }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := config.Paths.ThreadsDB, filepath.Join(filepath.Dir(history), "nano_threads.sqlite"); got != want {
		t.Fatalf("threads path = %q, want %q", got, want)
	}
	if got, want := config.Paths.MailDB, filepath.Join(filepath.Dir(history), "mail.sqlite"); got != want {
		t.Fatalf("mail path = %q, want %q", got, want)
	}
	if got, want := config.Paths.RemoteAuthDB, filepath.Join(home, ".nanod", "remote-auth.sqlite"); got != want {
		t.Fatalf("remote auth path = %q, want %q", got, want)
	}
}

// Catches: a Codex child started without a sandbox field getting a read-only
// sandbox, against the permissive default launch policy.
// Boundary: Nano's flag defaults, which the launchd job leaves unset, through
// the start resolver that fills an omitted sandbox and approval.
func TestCodexStartWithoutSandboxGetsPermissiveDefault(t *testing.T) {
	config, err := parseConfigWith(nil, func() (string, error) { return t.TempDir(), nil }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	s := &server{defaults: sessionDefaults{cwd: t.TempDir(), sandbox: config.Sandbox, approval: config.Approval}}
	req, err := s.resolveSpawn(spawnRequest{Provider: "codex", Mode: "start"})
	if err != nil {
		t.Fatal(err)
	}
	if req.Sandbox != "danger-full-access" || req.Approval != "never" {
		t.Fatalf("omitted Codex posture resolved to sandbox=%q approval=%q", req.Sandbox, req.Approval)
	}
}
