package history

// LegacyNanoIdentity is persisted in provider origins and hashed into submission
// IDs. Changing it would reclassify old history and orphan in-flight submissions.
const LegacyNanoIdentity = "codex-nano"

// Interaction is the deterministic session-interaction class derived from the
// provider-native origin string. It is stored on the thread row so list
// filtering is a cheap WHERE clause; the verbatim Origin remains the source of
// truth and is always recomputed on reindex.
type Interaction string

const (
	InteractionInteractive  Interaction = "interactive"
	InteractionProgrammatic Interaction = "programmatic"
	InteractionUnclassified Interaction = "unclassified"
)

// originInteractionClass maps provider-native origin values to interaction
// classes. Any origin absent from this table, including unknown origins, is
// unclassified: hiding a run requires positive programmatic evidence, never
// a heuristic.
var originInteractionClass = map[Provider]map[string]Interaction{
	ProviderClaude: {
		"cli": InteractionInteractive,
		// Nano signs the children it spawns with CLAUDE_CODE_ENTRYPOINT so its
		// own operator-facing sessions are never mistaken for `sdk-cli` batch
		// runs (claude.go, cmd/claude-shim/session.go).
		"nano":             InteractionInteractive,
		LegacyNanoIdentity: InteractionInteractive,
		"sdk-cli":          InteractionProgrammatic,
	},
	ProviderCodex: {
		"codex-tui":              InteractionInteractive,
		"codex_cli_rs":           InteractionInteractive,
		"codex_vscode":           InteractionInteractive,
		"nano":                   InteractionInteractive,
		LegacyNanoIdentity:       InteractionInteractive,
		"mobile-terminal-v2":     InteractionInteractive,
		"mobile-terminal-v2-ts":  InteractionInteractive,
		"mobile-terminal-v3-mvp": InteractionInteractive,
		"codex_exec":             InteractionProgrammatic,
	},
}

// ClassifyInteraction derives the interaction class from a thread's provider
// and origin field. An origin that is not a known value classifies as
// unclassified, which is always shown.
func ClassifyInteraction(provider Provider, origin MetadataField) Interaction {
	if origin.State != MetadataKnown || origin.Value == nil {
		return InteractionUnclassified
	}
	if byOrigin, ok := originInteractionClass[provider]; ok {
		if class, ok := byOrigin[*origin.Value]; ok {
			return class
		}
	}
	return InteractionUnclassified
}
