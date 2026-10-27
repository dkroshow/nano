package history

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"testing"
)

// These public fixture expectations were captured from the removed metadata
// readers. The module and its reducer must continue to match them.
func catalogFixtureOracle(t *testing.T, provider Provider, req ProjectionRequest) (ProjectionResult, error) {
	t.Helper()
	raw, err := io.ReadAll(req.Reader)
	if err != nil {
		return ProjectionResult{}, err
	}
	key := fmt.Sprintf("%s/%s/%x", t.Name(), provider, sha256.Sum256(raw))
	path := "testdata/catalog-compatibility.json"
	data, err := os.ReadFile(path)
	oracle := map[string]ProjectionResult{}
	if err == nil {
		if err = json.Unmarshal(data, &oracle); err != nil {
			return ProjectionResult{}, err
		}
	} else {
		return ProjectionResult{}, err
	}
	result, ok := oracle[key]
	if !ok {
		return ProjectionResult{}, fmt.Errorf("missing frozen catalog fixture %s", key)
	}
	return result, nil
}
