package sessions

import (
	"testing"
)

func TestPermissionInteractionClassification(t *testing.T) {
	tests := []struct {
		name string
		p    pendingInteraction
		want bool
	}{
		{"codex command", pendingInteraction{Kind: kindCommandApproval}, true},
		{"codex file", pendingInteraction{Kind: kindFileChangeApproval}, true},
		{"claude tool", pendingInteraction{Kind: kindToolPermission}, true},
		{"grok permission", pendingInteraction{Method: grokPermissionMethod}, true},
		{"grok question", pendingInteraction{Method: grokAskQuestionMethod, Kind: kindToolPermission}, false},
		{"user input", pendingInteraction{Kind: kindUserInput}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isPermissionInteraction(test.p); got != test.want {
				t.Fatalf("isPermissionInteraction = %v, want %v", got, test.want)
			}
		})
	}
}
