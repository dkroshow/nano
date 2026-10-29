package sessions

import (
	"testing"

	"github.com/dkroshow/nano/internal/interact"
)

func TestPermissionInteractionClassification(t *testing.T) {
	tests := []struct {
		name string
		p    interact.PendingInteraction
		want bool
	}{
		{"codex command", interact.PendingInteraction{Kind: kindCommandApproval}, true},
		{"codex file", interact.PendingInteraction{Kind: kindFileChangeApproval}, true},
		{"claude tool", interact.PendingInteraction{Kind: kindToolPermission}, true},
		{"grok permission", interact.PendingInteraction{Method: grokPermissionMethod}, true},
		{"grok question", interact.PendingInteraction{Method: grokAskQuestionMethod, Kind: kindToolPermission}, false},
		{"user input", interact.PendingInteraction{Kind: kindUserInput}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isPermissionInteraction(test.p); got != test.want {
				t.Fatalf("isPermissionInteraction = %v, want %v", got, test.want)
			}
		})
	}
}
