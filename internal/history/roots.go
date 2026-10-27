package history

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RootOptions provides startup evidence and injectable environment access.
// CodexHomeFromInitialize is authoritative when non-empty.
type RootOptions struct {
	HomeDir                 string
	CodexHomeFromInitialize string
	LookupEnv               func(string) (string, bool)
}

// ResolveStandardRoots resolves the provider roots supported for discovery:
// Codex active and archived sessions, Claude projects, and Grok sessions.
func ResolveStandardRoots(opts RootOptions) ([]SourceRoot, error) {
	home := strings.TrimSpace(opts.HomeDir)
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve user home: %w", err)
		}
	}
	home, err := canonicalPath(home, home)
	if err != nil {
		return nil, fmt.Errorf("canonicalize user home: %w", err)
	}

	lookup := opts.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}

	codexHome := strings.TrimSpace(opts.CodexHomeFromInitialize)
	if codexHome == "" {
		if value, ok := lookup("CODEX_HOME"); ok {
			codexHome = strings.TrimSpace(value)
		}
	}
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	codexHome, err = canonicalPath(codexHome, home)
	if err != nil {
		return nil, fmt.Errorf("canonicalize Codex home: %w", err)
	}

	claudeHome := ""
	if value, ok := lookup("CLAUDE_CONFIG_DIR"); ok {
		claudeHome = strings.TrimSpace(value)
	}
	if claudeHome == "" {
		claudeHome = filepath.Join(home, ".claude")
	}
	claudeHome, err = canonicalPath(claudeHome, home)
	if err != nil {
		return nil, fmt.Errorf("canonicalize Claude home: %w", err)
	}

	codexSessions, err := canonicalPath(filepath.Join(codexHome, "sessions"), home)
	if err != nil {
		return nil, fmt.Errorf("canonicalize Codex sessions root: %w", err)
	}
	codexArchived, err := canonicalPath(filepath.Join(codexHome, "archived_sessions"), home)
	if err != nil {
		return nil, fmt.Errorf("canonicalize Codex archived root: %w", err)
	}
	claudeProjects, err := canonicalPath(filepath.Join(claudeHome, "projects"), home)
	if err != nil {
		return nil, fmt.Errorf("canonicalize Claude projects root: %w", err)
	}

	grokSessions, err := canonicalPath(filepath.Join(home, ".grok", "sessions"), home)
	if err != nil {
		return nil, fmt.Errorf("canonicalize Grok sessions root: %w", err)
	}

	return []SourceRoot{
		{Provider: ProviderCodex, Kind: RootCodexSessions, Path: codexSessions},
		{Provider: ProviderCodex, Kind: RootCodexArchived, Path: codexArchived, Archived: true},
		{Provider: ProviderClaude, Kind: RootClaudeProjects, Path: claudeProjects},
		{Provider: ProviderGrok, Kind: RootGrokSessions, Path: grokSessions},
	}, nil
}

func canonicalPath(path, home string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "~" {
		path = home
	} else if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		path = filepath.Join(home, path[2:])
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	resolved, err := filepath.EvalSymlinks(absolute)
	if err == nil {
		return resolved, nil
	}
	if os.IsNotExist(err) {
		return absolute, nil
	}
	return "", err
}
