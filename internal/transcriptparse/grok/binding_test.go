package grok

import (
	"reflect"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

func TestPromptBindingEmitsOrderedIndependentRevisions(t *testing.T) {
	raw := []byte(`{"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"Prompt"}},"_meta":{"eventId":"e1"}}}`)
	state, _, err := ParseWire(State{}, inputBytes(raw))
	if err != nil {
		t.Fatal(err)
	}
	var user model.Item
	for _, item := range state.Items {
		user = item
	}
	// Two assembled facets in a checkpoint must yield two distinct upserts when
	// their scope is bound. The source-order contract also applies to revisions.
	extra := user
	extra.ID, extra.Scope.ItemID, extra.SourceSeq = "additional-facet", "additional-facet", 2
	extra.Type, extra.Role, extra.Observations = model.ItemNotice, model.RoleSystem, nil
	state.Items[extra.ID] = extra
	scope := user.Scope
	scope.Kind, scope.ItemID = model.ScopeTurn, ""
	prompt := "p1"
	scope.NativeTurnID = &prompt
	var batch model.Batch
	state.bindScope(&batch, scope, decodedUpdate{Kind: kindAgentMessage, SessionID: "s1", PromptID: prompt}, model.SourceRef{Kind: "wire", Label: "fixture", Incarnation: "capture-1", Frame: 2})
	var ids []string
	for _, change := range batch.Changes {
		if change.Item != nil {
			ids = append(ids, change.Item.ID)
			if change.Item.Scope.NativeTurnID == nil || *change.Item.Scope.NativeTurnID != prompt {
				t.Fatal("binding revision omitted native prompt")
			}
		}
	}
	if !reflect.DeepEqual(ids, []string{user.ID, extra.ID}) {
		t.Fatalf("binding revisions=%v", ids)
	}
}
