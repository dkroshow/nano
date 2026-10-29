package agentapi

import (
	"path/filepath"
	"testing"
	"time"

	featureheartbeat "github.com/dkroshow/nano/internal/features/heartbeat"
	"github.com/dkroshow/nano/internal/interact"
	"github.com/dkroshow/nano/internal/threads"
)

func openTestHeartbeatStore(t *testing.T) *featureheartbeat.Store {
	t.Helper()
	store, err := featureheartbeat.OpenStore(filepath.Join(t.TempDir(), "heartbeat.sqlite"), threads.Backup)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
func TestHeartbeatTemplatesAreRepoOwnedAndCopiesArePerAttachment(t *testing.T) {
	templates, err := featureheartbeat.LoadScriptTemplates()
	if err != nil || len(templates) != 1 || templates[0].ID != "inbox_match" {
		t.Fatalf("templates = %#v, %v", templates, err)
	}
	store := openTestHeartbeatStore(t)
	s := &Controller{heartbeat: featureheartbeat.NewService(store, interact.NewHTTPClient("", nil), true, nil)}
	actor := featureheartbeat.Actor{Kind: "operator", Name: "Operator"}
	first, _, err := s.copyHeartbeatTemplate(heartbeatPrePingTemplateRequest{TemplateID: "inbox_match"}, actor, "approved", "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := s.copyHeartbeatTemplate(heartbeatPrePingTemplateRequest{TemplateID: "inbox_match"}, actor, "approved", "", time.Now())
	if err != nil || first.ScriptID == second.ScriptID {
		t.Fatalf("copies = %#v %#v, %v", first, second, err)
	}
	revision, err := store.ScriptRevision(first.ScriptID, first.Revision)
	if err != nil || revision.TemplateID != "inbox_match" || revision.Source == "" {
		t.Fatalf("revision = %#v, %v", revision, err)
	}
}
