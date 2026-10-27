package transcriptparse_test

import (
	"errors"
	"math"
	"testing"

	"github.com/dkroshow/nano/internal/transcriptparse/claude"
	"github.com/dkroshow/nano/internal/transcriptparse/codex"
	"github.com/dkroshow/nano/internal/transcriptparse/grok"
	"github.com/dkroshow/nano/internal/transcriptparse/model"
)

// A closed capture and an overflowing byte coordinate need different caller
// recovery. Neither defect should masquerade as a generic corrupt checkpoint.
func TestFileReadersPreserveFramingErrorCodes(t *testing.T) {
	for _, provider := range []string{"claude", "codex", "grok"} {
		for _, want := range []string{"source_closed", "offset_overflow"} {
			t.Run(provider+"/"+want, func(t *testing.T) {
				input := model.Input{Source: model.SourceRef{Label: "framing-error", Incarnation: "1"}, Closed: want == "source_closed"}
				var err error
				switch provider {
				case "claude":
					state, _, first := claude.ParseFile(claude.State{}, input)
					if first != nil {
						t.Fatal(first)
					}
					if want == "offset_overflow" {
						state.Framing.Offset = math.MaxInt64 - 1
						state.Framing.Checkpoint = state.Framing.Offset
					}
					input.Bytes = []byte("{}")
					_, _, err = claude.ParseFile(state, input)
				case "codex":
					state, _, first := codex.ParseFile(codex.State{}, input)
					if first != nil {
						t.Fatal(first)
					}
					if want == "offset_overflow" {
						state.Framing.Offset = math.MaxInt64 - 1
						state.Framing.Checkpoint = state.Framing.Offset
					}
					input.Bytes = []byte("{}")
					_, _, err = codex.ParseFile(state, input)
				case "grok":
					state, _, first := grok.ParseFile(grok.State{}, input)
					if first != nil {
						t.Fatal(first)
					}
					if want == "offset_overflow" {
						state.Framing.Offset = math.MaxInt64 - 1
						state.Framing.Checkpoint = state.Framing.Offset
					}
					input.Bytes = []byte("{}")
					_, _, err = grok.ParseFile(state, input)
				}
				var diagnostic model.Diagnostic
				if !errors.As(err, &diagnostic) || diagnostic.Code != want || diagnostic.Severity != "error" || diagnostic.Source.Label != input.Source.Label {
					t.Fatalf("got %v; want scoped %s error", err, want)
				}
			})
		}
	}
}
