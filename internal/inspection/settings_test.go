package inspection

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadClaudeSettingsUsesOnlyExplicitFixtureLayers(t *testing.T) {
	root := t.TempDir()
	user := filepath.Join(root, "user.json")
	project := filepath.Join(root, "project.json")
	if err := os.WriteFile(user, []byte(`{"model":"user","effort":"low","permissions":{"allow":["Read"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(project, []byte(`{"model":"project","effortLevel":"high","permissions":{"deny":["Read(secret)"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	settings, diagnostics := ReadClaudeSettings([]string{user, filepath.Join(root, "missing.json"), project})
	if len(diagnostics) != 0 {
		t.Fatalf("diagnostics = %v", diagnostics)
	}
	if settings.Model != "project" || settings.Effort != "high" {
		t.Fatalf("settings = %#v", settings)
	}
	if len(settings.Permissions.Allow) != 1 || len(settings.Permissions.Deny) != 1 {
		t.Fatalf("permissions = %#v", settings.Permissions)
	}
}

func TestReadClaudeSettingsReportsMalformedFixture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, diagnostics := ReadClaudeSettings([]string{path})
	if len(diagnostics) != 1 || !strings.Contains(diagnostics[0], "Parse Claude settings") {
		t.Fatalf("diagnostics = %v", diagnostics)
	}
}

func TestKnownMetadataValueRequiresKnownEvidence(t *testing.T) {
	value := "  /repo  "
	if got := KnownMetadataValue("known", &value); got != "/repo" {
		t.Fatalf("known value = %q", got)
	}
	if got := KnownMetadataValue("unknown", &value); got != "" {
		t.Fatalf("unknown value = %q", got)
	}
}
