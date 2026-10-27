package setup

// InstructionFileSource describes a file and the evidence for its inclusion in context.
type InstructionFileSource struct {
	Path         string
	SourceKind   string
	SourceScope  string
	SourceLoad   string
	ProvenLoaded bool
	Evidence     string
}

const (
	// InstructionEvidenceWire marks sources attested by provider runtime evidence.
	InstructionEvidenceWire = "wire-authoritative"
	// InstructionEvidenceDocumented marks sources inferred from documented loading rules.
	InstructionEvidenceDocumented = "documented-expected"
	// InstructionEvidencePresent marks sources known only to exist on disk.
	InstructionEvidencePresent = "present-only"
)
