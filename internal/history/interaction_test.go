package history

import "testing"

func knownOrigin(value string) MetadataField {
	evidence := EvidenceNativeDurable
	return MetadataField{State: MetadataKnown, Value: &value, Evidence: &evidence}
}

func TestClassifyInteraction(t *testing.T) {
	cases := []struct {
		name     string
		provider Provider
		origin   MetadataField
		want     Interaction
	}{
		{"claude cli interactive", ProviderClaude, knownOrigin("cli"), InteractionInteractive},
		{"claude sdk-cli programmatic", ProviderClaude, knownOrigin("sdk-cli"), InteractionProgrammatic},
		{"claude nano interactive", ProviderClaude, knownOrigin("nano"), InteractionInteractive},
		{"claude legacy nano interactive", ProviderClaude, knownOrigin(LegacyNanoIdentity), InteractionInteractive},
		{"codex tui interactive", ProviderCodex, knownOrigin("codex-tui"), InteractionInteractive},
		{"codex cli_rs interactive", ProviderCodex, knownOrigin("codex_cli_rs"), InteractionInteractive},
		{"codex vscode interactive", ProviderCodex, knownOrigin("codex_vscode"), InteractionInteractive},
		{"codex nano interactive", ProviderCodex, knownOrigin("nano"), InteractionInteractive},
		{"codex legacy nano interactive", ProviderCodex, knownOrigin(LegacyNanoIdentity), InteractionInteractive},
		{"codex mtv2 interactive", ProviderCodex, knownOrigin("mobile-terminal-v2"), InteractionInteractive},
		{"codex mtv2-ts interactive", ProviderCodex, knownOrigin("mobile-terminal-v2-ts"), InteractionInteractive},
		{"codex mtv3 interactive", ProviderCodex, knownOrigin("mobile-terminal-v3-mvp"), InteractionInteractive},
		{"codex exec programmatic", ProviderCodex, knownOrigin("codex_exec"), InteractionProgrammatic},
		{"codex probe unclassified", ProviderCodex, knownOrigin("mtv5-probe"), InteractionUnclassified},
		{"codex unrecognized unclassified", ProviderCodex, knownOrigin("something-new"), InteractionUnclassified},
		{"claude unrecognized unclassified", ProviderClaude, knownOrigin("vscode"), InteractionUnclassified},
		{"unknown origin unclassified", ProviderClaude, UnknownField(), InteractionUnclassified},
		{"unsupported origin unclassified", ProviderCodex, UnsupportedField(), InteractionUnclassified},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyInteraction(tc.provider, tc.origin); got != tc.want {
				t.Fatalf("ClassifyInteraction(%q, %#v) = %q, want %q", tc.provider, tc.origin, got, tc.want)
			}
		})
	}
}
