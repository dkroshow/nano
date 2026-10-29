package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dkroshow/nano/internal/configpaths"
)

// LiveTextDeltasDisabled is an environment switch, not a flag, so a rollback
// to a binary that predates it still boots with the same launch plist.
// LiveTextDeltasDisabled reads the rollback-compatible live-text switch.
func LiveTextDeltasDisabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("NANO_LIVE_TEXT_DELTAS")), "off")
}

// SupersededProjectionCollectionEnabled requires an explicit collection opt-in.
func SupersededProjectionCollectionEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("NANO_SUPERSEDED_COLLECTION")), "on")
}

// ProviderHomes resolves the read-only roots owned by provider processes.
func ProviderHomes() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve provider home: %w", err)
	}
	codexHome := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	claudeHome := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	if claudeHome == "" {
		claudeHome = filepath.Join(home, ".claude")
	}
	grokHome := filepath.Join(home, ".grok")
	result := make([]string, 0, 3)
	for _, providerHome := range []string{codexHome, claudeHome, grokHome} {
		canonical, err := configpaths.CanonicalProspective(providerHome)
		if err != nil {
			return nil, fmt.Errorf("resolve provider-owned directory: %w", err)
		}
		result = append(result, canonical)
	}
	return result, nil
}

// ValidateCatalogLocation rejects storing the index inside a provider-owned tree.
func ValidateCatalogLocation(database string, providerHomes []string) error {
	canonicalDatabase, err := configpaths.CanonicalProspective(database)
	if err != nil {
		return fmt.Errorf("resolve thread catalog database location: %w", err)
	}
	for _, providerHome := range providerHomes {
		canonicalHome, err := configpaths.CanonicalProspective(providerHome)
		if err != nil {
			return fmt.Errorf("resolve provider-owned directory: %w", err)
		}
		relative, err := filepath.Rel(canonicalHome, canonicalDatabase)
		if err != nil {
			return fmt.Errorf("compare thread catalog and provider paths: %w", err)
		}
		if relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
			return fmt.Errorf("thread catalog database must not be inside provider-owned directory %s", canonicalHome)
		}
	}
	return nil
}
