package app

import (
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/dkroshow/nano/internal/threadindex"
)

// Enhanced shows user and agent messages only; Dev shows everything.
func TestEnhancedDefaultShowsOnlyMessages(t *testing.T) {
	for _, view := range []string{"enhanced", "dev"} {
		query, err := threadindex.ParseTranscriptListQuery(httptest.NewRequest("GET", "/?view="+view, nil))
		if err != nil {
			t.Fatal(err)
		}
		want := []string{}
		if view == "enhanced" {
			want = []string{"reasoning", "toolCall", "toolResult", "subagent", "system", "error", "unclassified"}
		}
		if !slices.Equal(query.HiddenCategories, want) {
			t.Fatalf("%s hidden categories = %v, want %v", view, query.HiddenCategories, want)
		}
	}
}
