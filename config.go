package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dkroshow/nano/internal/configpaths"
)

// Config is Nano's complete process configuration. Every filesystem write
// target is explicit after ParseConfig returns, so an isolated candidate can
// run without sharing the installed server's data, logs, or transient spool.
type Config struct {
	Port                     int
	WorkingDirectory         string
	Sandbox                  string
	Approval                 string
	Model                    string
	ReasoningSummary         string
	CodexWS                  string
	CodexSupervisorLabel     string
	ClaudeBin                string
	ClaudeShimWS             string
	ClaudeShimSpoolDir       string
	GrokBin                  string
	GrokLeaderSocket         string
	AllowedTools             string
	DisableWatch             bool
	HeartbeatEnabled         bool
	HeartbeatMaxWakesPerHour int
	MailRetentionDays        int
	UsageEnabled             bool
	UsagePollInterval        time.Duration
	RemoteAuthPort           int
	RemoteAuthHost           string
	LocalTLSPort             int
	// FileTranscripts retains the accepted compatibility flag. All providers
	// use file-only transcripts regardless of its value.
	FileTranscripts []string
	Paths           ConfigPaths
}

// ConfigPaths names every store, log, migration source, backup root, and
// transient spool used by the Nano server process.
type ConfigPaths struct {
	DataDir                  string
	HistoryDB                string
	ThreadNames              string
	ThreadsDB                string
	LegacyControlDB          string
	LegacyQueuedTurns        string
	LegacySendAttempts       string
	ThreadsBackups           string
	Attachments              string
	HeartbeatDB              string
	MailDB                   string
	CompletedTurnsDB         string
	ThreadStarsDB            string
	ThreadPinsDB             string
	FeedbackDB               string
	UsageDB                  string
	RemoteAuthDB             string
	MaintenanceBackups       string
	ClaudeMaintenanceReceipt string
	ServerLog                string
	WireSpoolDir             string
}

type configPathFlags ConfigPaths

func parseConfig(args []string) (Config, error) {
	return parseConfigWith(args, os.UserHomeDir, os.Stderr)
}

func parseConfigWith(args []string, userHomeDir func() (string, error), output io.Writer) (Config, error) {
	var config Config
	var paths configPathFlags
	flags := flag.NewFlagSet("nanod", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.IntVar(&config.Port, "port", 7690, "listen port")
	flags.StringVar(&config.WorkingDirectory, "cwd", ".", "default working directory for new sessions")
	flags.StringVar(&config.Sandbox, "sandbox", "danger-full-access", "codex sandbox mode when a start omits it: read-only | workspace-write | danger-full-access")
	flags.StringVar(&config.Approval, "approval", "never", "codex approval policy: untrusted | on-failure | on-request | never")
	flags.StringVar(&config.Model, "model", "", "default model override (default: provider config)")
	flags.StringVar(&config.ReasoningSummary, "reasoning-summary", "detailed", "codex reasoning summary level: auto | concise | detailed | none")
	flags.StringVar(&config.CodexWS, "codex-ws", "ws://127.0.0.1:7691", "codex app-server daemon websocket URL")
	flags.StringVar(&config.CodexSupervisorLabel, "codex-supervisor-label", supervisedDaemonLabel, "launchd label of the supervised Codex app-server")
	flags.StringVar(&config.ClaudeBin, "claude-bin", "", "claude executable path (default: CODEX_NANO_CLAUDE_BIN, ~/.local/bin/claude, Homebrew, then PATH)")
	flags.StringVar(&config.ClaudeShimWS, "claude-shim-ws", defaultClaudeShimWS, "claude shim daemon websocket URL; empty spawns claude children directly")
	flags.StringVar(&config.ClaudeShimSpoolDir, "claude-shim-spool-dir", "", "the claude shim daemon's spool directory, read before re-attach (default <data-dir>/claude-shim/spools)")
	flags.StringVar(&config.GrokBin, "grok-bin", "", "grok executable path (default: CODEX_NANO_GROK_BIN, ~/.grok/bin/grok, ~/.local/bin/grok, then PATH)")
	flags.StringVar(&config.GrokLeaderSocket, "grok-leader-socket", "", "Grok native leader socket; empty uses ~/.grok/leader.sock")
	flags.StringVar(&config.AllowedTools, "allowed-tools", "Read,Grep,Glob", "claude allowed tools")
	flags.BoolVar(&config.DisableWatch, "no-watch", false, "watch no attached-thread files; attached threads then advance only on provider hints and explicit refresh")
	flags.BoolVar(&config.HeartbeatEnabled, "heartbeat", false, "enable the wall-clock heartbeat scheduler")
	flags.IntVar(&config.HeartbeatMaxWakesPerHour, "heartbeat-max-wakes-per-hour", defaultHeartbeatMaxWakesPerHour, "global maximum heartbeat wakes in a rolling hour")
	flags.IntVar(&config.MailRetentionDays, "mail-retention-days", -1, "days to keep dispositioned mail; -1 preserves the stored setting")
	flags.BoolVar(&config.UsageEnabled, "usage", true, "poll and retain account usage")
	flags.DurationVar(&config.UsagePollInterval, "usage-poll-interval", defaultUsageLimitPollInterval, "account usage polling interval")
	flags.IntVar(&config.RemoteAuthPort, "remote-auth-port", 0, "protected loopback port for a Tailscale Serve proxy")
	flags.StringVar(&config.RemoteAuthHost, "remote-auth-host", "", "exact public Host accepted by the protected remote listener")
	flags.IntVar(&config.LocalTLSPort, "local-tls-port", 0, "trusted loopback HTTPS port, using <data-dir>/tls/localhost.pem and localhost-key.pem")
	fileTranscripts := flags.String("file-transcripts", "", "accepted compatibility flag (codex,claude,grok); all providers always use file-only transcripts")

	flags.StringVar(&paths.DataDir, "data-dir", "", "base for unspecified Nano data paths (default: ~/.nanod)")
	flags.StringVar(&paths.HistoryDB, "history-db", "", "thread catalog SQLite path")
	flags.StringVar(&paths.ThreadNames, "thread-names", "", "thread names truth file")
	flags.StringVar(&paths.ThreadsDB, "threads-db", "", "canonical hosted-thread SQLite path")
	flags.StringVar(&paths.LegacyControlDB, "legacy-control-db", "", "legacy hosted metadata SQLite migration source")
	flags.StringVar(&paths.LegacyQueuedTurns, "legacy-queued-turns", "", "legacy queued-turn JSON migration source")
	flags.StringVar(&paths.LegacySendAttempts, "legacy-send-attempts", "", "legacy send-attempt JSON migration source")
	flags.StringVar(&paths.ThreadsBackups, "threads-backup-dir", "", "hosted-thread migration backup directory")
	flags.StringVar(&paths.Attachments, "attachments-store", "", "legacy hosted attachment JSON store")
	flags.StringVar(&paths.HeartbeatDB, "heartbeat-db", "", "heartbeat SQLite path")
	flags.StringVar(&paths.MailDB, "mail-db", "", "mail SQLite path")
	flags.StringVar(&paths.CompletedTurnsDB, "completed-turns-db", "", "legacy completed-turn SQLite path")
	flags.StringVar(&paths.ThreadStarsDB, "thread-stars-db", "", "thread star SQLite path")
	flags.StringVar(&paths.ThreadPinsDB, "thread-pins-db", "", "thread transcript pin SQLite path")
	flags.StringVar(&paths.FeedbackDB, "feedback-db", "", "operator feedback note SQLite path")
	flags.StringVar(&paths.UsageDB, "usage-db", "", "account usage SQLite path")
	flags.StringVar(&paths.RemoteAuthDB, "remote-auth-db", "", "remote password and browser-session SQLite path")
	flags.StringVar(&paths.MaintenanceBackups, "maintenance-backup-dir", "", "submission-maintenance backup directory")
	flags.StringVar(&paths.ClaudeMaintenanceReceipt, "claude-maintenance-receipt", "", "Claude submission-maintenance receipt file")
	flags.StringVar(&paths.ServerLog, "server-log", "", "persistent server log path")
	flags.StringVar(&paths.WireSpoolDir, "wire-spool-dir", "", "transient active-session event spool directory")

	if err := flags.Parse(args); err != nil {
		return Config{}, err
	}
	for _, provider := range strings.Split(*fileTranscripts, ",") {
		switch provider = strings.TrimSpace(provider); provider {
		case "":
		case "claude", "codex", "grok":
			config.FileTranscripts = append(config.FileTranscripts, provider)
		default:
			return Config{}, fmt.Errorf("file-transcripts: unsupported provider %q", provider)
		}
	}
	home, err := userHomeDir()
	if err != nil {
		return Config{}, fmt.Errorf("resolve home for Nano paths: %w", err)
	}
	config.Paths = resolveConfigPaths(ConfigPaths(paths), home)
	// The separately supervised shim must use the same directory.
	config.ClaudeShimSpoolDir = pathOr(config.ClaudeShimSpoolDir, filepath.Join(config.Paths.DataDir, "claude-shim", "spools"))
	return config, nil
}

func resolveConfigPaths(paths ConfigPaths, home string) ConfigPaths {
	base := strings.TrimSpace(paths.DataDir)
	if base == "" {
		base = configpaths.DataDir(home)
	}
	paths.DataDir = base
	paths.HistoryDB = pathOr(paths.HistoryDB, filepath.Join(base, "history.sqlite"))
	historyDir := filepath.Dir(paths.HistoryDB)
	paths.ThreadNames = pathOr(paths.ThreadNames, filepath.Join(historyDir, "thread-names.json"))
	paths.ThreadsDB = pathOr(paths.ThreadsDB, filepath.Join(historyDir, "nano_threads.sqlite"))
	paths.LegacyControlDB = pathOr(paths.LegacyControlDB, filepath.Join(historyDir, "control.sqlite"))
	paths.LegacyQueuedTurns = pathOr(paths.LegacyQueuedTurns, filepath.Join(historyDir, "queued-turns.json"))
	paths.LegacySendAttempts = pathOr(paths.LegacySendAttempts, filepath.Join(historyDir, "send-attempts.json"))
	paths.ThreadsBackups = pathOr(paths.ThreadsBackups, filepath.Join(historyDir, "threads-backups"))
	paths.Attachments = pathOr(paths.Attachments, filepath.Join(historyDir, "attachments.json"))
	paths.MailDB = pathOr(paths.MailDB, filepath.Join(historyDir, "mail.sqlite"))
	paths.UsageDB = pathOr(paths.UsageDB, filepath.Join(historyDir, "usage-limits.sqlite"))
	paths.MaintenanceBackups = pathOr(paths.MaintenanceBackups, filepath.Join(historyDir, "maintenance-backups"))
	paths.ClaudeMaintenanceReceipt = pathOr(paths.ClaudeMaintenanceReceipt, filepath.Join(historyDir, "claude-submission-maintenance.json"))
	paths.CompletedTurnsDB = pathOr(paths.CompletedTurnsDB, filepath.Join(base, "turn-completed.sqlite"))
	paths.HeartbeatDB = pathOr(paths.HeartbeatDB, filepath.Join(base, "heartbeat.sqlite"))
	paths.ThreadStarsDB = pathOr(paths.ThreadStarsDB, filepath.Join(base, "thread-stars.sqlite"))
	paths.ThreadPinsDB = pathOr(paths.ThreadPinsDB, filepath.Join(base, "thread-pins.sqlite"))
	paths.FeedbackDB = pathOr(paths.FeedbackDB, filepath.Join(base, "feedback.sqlite"))
	paths.RemoteAuthDB = pathOr(paths.RemoteAuthDB, filepath.Join(base, "remote-auth.sqlite"))
	paths.ServerLog = pathOr(paths.ServerLog, filepath.Join(base, "logs", "server.log"))
	paths.WireSpoolDir = pathOr(paths.WireSpoolDir, filepath.Join(base, "wire"))
	return paths
}

func pathOr(value, fallback string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return fallback
}
