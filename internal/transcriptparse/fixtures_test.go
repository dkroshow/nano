package transcriptparse_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/dkroshow/nano/internal/transcriptparse/internal/jsonl"
)

type researchCase struct {
	Name              string
	Manifest          string
	Expected          map[string]json.RawMessage
	Inputs            map[string][]byte
	Recipes           map[string]json.RawMessage
	AssertionsOnly    bool
	ExternalFinalRefs []string
}

func TestResearchFixtureInventoryAndFraming(t *testing.T) {
	for _, provider := range []string{"codex", "claude", "grok"} {
		t.Run(provider, func(t *testing.T) {
			root := os.DirFS(provider + "/testdata")
			data, err := fs.ReadFile(root, "research-inventory.json")
			if err != nil {
				t.Fatal(err)
			}
			var inventory struct {
				Cases  []string          `json:"cases"`
				SHA256 map[string]string `json:"sha256"`
			}
			if err := json.Unmarshal(data, &inventory); err != nil {
				t.Fatal(err)
			}
			fixtures, err := fs.Sub(root, "research")
			if err != nil {
				t.Fatal(err)
			}
			var actualCases []string
			err = fs.WalkDir(fixtures, ".", func(name string, entry fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if entry.IsDir() {
					if name != "." && !strings.Contains(name, "/") {
						actualCases = append(actualCases, name)
					}
					return nil
				}
				data, err := fs.ReadFile(fixtures, name)
				if err != nil {
					return err
				}
				hash := sha256.Sum256(data)
				if want, ok := inventory.SHA256[name]; !ok || hex.EncodeToString(hash[:]) != want {
					t.Errorf("research source changed or not inventoried: %s", name)
				}
				delete(inventory.SHA256, name)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(inventory.SHA256) != 0 {
				t.Errorf("missing inventoried research files: %v", inventory.SHA256)
			}
			if !reflect.DeepEqual(actualCases, inventory.Cases) {
				t.Fatal("case directories differ from the research inventory")
			}
			for _, name := range actualCases {
				t.Run(name, func(t *testing.T) {
					fixture, err := loadResearchCase(fixtures, name)
					if err != nil {
						t.Fatal(err)
					}
					for name, input := range fixture.Inputs {
						if strings.HasSuffix(name, ".jsonl") {
							checkFixtureFraming(t, input)
						}
					}
				})
			}
			t.Logf("%d research cases: source integrity and framing only; semantic conformance is not asserted", len(actualCases))
		})
	}
}

func TestFixtureLoaderRejectsMissingOrInvalidEvidence(t *testing.T) {
	valid := fstest.MapFS{
		"case/manifest.md":   {Data: []byte("Constructed example; partial assertions.")},
		"case/expected.json": {Data: []byte(`{"items":[],"turns":[],"diagnostics":[]}`)},
		"case/input.jsonl":   {Data: []byte("{bad}\n{\"partial\"")},
	}
	for _, name := range []string{"manifest", "expected", "input", "invalid_expected", "missing_array", "duplicate_id", "missing_final_ref"} {
		t.Run(name, func(t *testing.T) {
			files := fstest.MapFS{}
			for k, v := range valid {
				files[k] = v
			}
			switch name {
			case "manifest":
				delete(files, "case/manifest.md")
			case "expected":
				delete(files, "case/expected.json")
			case "input":
				delete(files, "case/input.jsonl")
			case "invalid_expected":
				files["case/expected.json"] = &fstest.MapFile{Data: []byte("{")}
			case "missing_array":
				files["case/expected.json"] = &fstest.MapFile{Data: []byte(`{"items":null,"turns":[],"diagnostics":[]}`)}
			case "duplicate_id":
				files["case/expected.json"] = &fstest.MapFile{Data: []byte(`{"items":[{"id":"a"},{"id":"a"}],"turns":[],"diagnostics":[]}`)}
			case "missing_final_ref":
				files["case/expected.json"] = &fstest.MapFile{Data: []byte(`{"items":[],"turns":[{"finalItemRefs":["absent"]}],"diagnostics":[]}`)}
			}
			if _, err := loadResearchCase(files, "case"); err == nil {
				t.Fatal("accepted missing or invalid fixture evidence")
			}
		})
	}
	fixture, err := loadResearchCase(valid, "case")
	if err != nil || !bytes.Equal(fixture.Inputs["input.jsonl"], valid["case/input.jsonl"].Data) {
		t.Fatalf("altered intentionally malformed input: %+v, %v", fixture, err)
	}
	delete(valid, "case/input.jsonl")
	valid["case/input.recipe.json"] = &fstest.MapFile{Data: []byte(`{"payloadBytes":1048577}`)}
	fixture, err = loadResearchCase(valid, "case")
	if err != nil || len(fixture.Inputs) != 0 || len(fixture.Recipes) != 1 {
		t.Fatalf("recipe confused with captured input: %+v, %v", fixture, err)
	}
	valid["case/expected.json"] = &fstest.MapFile{Data: []byte(`{"items":[],"turns":[{"finalItemRefs":["external"]}],"diagnostics":[],"assertionsOnly":true}`)}
	fixture, err = loadResearchCase(valid, "case")
	if err != nil || !fixture.AssertionsOnly || !reflect.DeepEqual(fixture.ExternalFinalRefs, []string{"external"}) {
		t.Fatalf("lost explicitly partial assertion: %+v %v", fixture, err)
	}
}

func loadResearchCase(root fs.FS, name string) (researchCase, error) {
	fixture := researchCase{Name: name, Inputs: map[string][]byte{}, Recipes: map[string]json.RawMessage{}}
	manifest, err := fs.ReadFile(root, path.Join(name, "manifest.md"))
	if err != nil {
		return fixture, err
	}
	fixture.Manifest = string(manifest)
	if strings.TrimSpace(fixture.Manifest) == "" {
		return fixture, fmt.Errorf("%s: empty manifest", name)
	}
	expected, err := fs.ReadFile(root, path.Join(name, "expected.json"))
	if err != nil {
		return fixture, err
	}
	if err := json.Unmarshal(expected, &fixture.Expected); err != nil {
		return fixture, err
	}
	if raw, ok := fixture.Expected["assertionsOnly"]; ok {
		if err := json.Unmarshal(raw, &fixture.AssertionsOnly); err != nil {
			return fixture, err
		}
	}
	for _, key := range []string{"items", "turns", "diagnostics"} {
		raw := bytes.TrimSpace(fixture.Expected[key])
		if len(raw) == 0 || raw[0] != '[' {
			return fixture, fmt.Errorf("%s: expected %s must be an array", name, key)
		}
	}
	var items []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(fixture.Expected["items"], &items); err != nil {
		return fixture, err
	}
	ids := map[string]bool{}
	for _, item := range items {
		if item.ID == "" || ids[item.ID] {
			return fixture, fmt.Errorf("%s: missing or repeated item id", name)
		}
		ids[item.ID] = true
	}
	var turns []struct {
		FinalItemRefs []string `json:"finalItemRefs"`
	}
	if err := json.Unmarshal(fixture.Expected["turns"], &turns); err != nil {
		return fixture, err
	}
	for _, turn := range turns {
		for _, id := range turn.FinalItemRefs {
			if !ids[id] {
				if !fixture.AssertionsOnly {
					return fixture, fmt.Errorf("%s: final item reference %q is absent", name, id)
				}
				fixture.ExternalFinalRefs = append(fixture.ExternalFinalRefs, id)
			}
		}
	}
	entries, err := fs.ReadDir(root, name)
	if err != nil {
		return fixture, err
	}
	for _, entry := range entries {
		file := entry.Name()
		if entry.IsDir() || file == "expected.json" || file == "manifest.md" {
			continue
		}
		data, err := fs.ReadFile(root, path.Join(name, file))
		if err != nil {
			return fixture, err
		}
		if strings.HasSuffix(file, ".json") && !json.Valid(data) {
			return fixture, fmt.Errorf("%s: invalid JSON document %s", name, file)
		}
		if strings.HasSuffix(file, ".recipe.json") {
			fixture.Recipes[file] = data
		} else {
			fixture.Inputs[file] = data
		}
	}
	if len(fixture.Inputs) == 0 && len(fixture.Recipes) == 0 {
		return fixture, fmt.Errorf("%s: no input or recipe", name)
	}
	return fixture, nil
}

func checkFixtureFraming(t *testing.T, input []byte) {
	t.Helper()
	state, want, err := jsonl.Feed(jsonl.State{}, input, 32<<20, false)
	if err != nil {
		t.Fatal(err)
	}
	// Exercise every delimiter and UTF-8 continuation boundary plus chunk edges.
	boundaries := map[int]bool{0: true, len(input): true, len(input) / 2: true}
	for i, b := range input {
		if b == '\n' || b == '\r' || b >= 0x80 {
			boundaries[i] = true
			boundaries[i+1] = true
		}
	}
	var cuts []int
	for cut := range boundaries {
		cuts = append(cuts, cut)
	}
	sort.Ints(cuts)
	for _, cut := range cuts {
		first, a, err := jsonl.Feed(jsonl.State{}, input[:cut], 32<<20, false)
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(first)
		var restored jsonl.State
		if err := json.Unmarshal(encoded, &restored); err != nil {
			t.Fatal(err)
		}
		next, b, err := jsonl.Feed(restored, input[cut:], 32<<20, false)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(append(a, b...), want) || !reflect.DeepEqual(next, state) {
			t.Fatalf("framing changed across cut %d", cut)
		}
	}
	for _, row := range want {
		raw := input[row.Start:row.End]
		raw = bytes.TrimSuffix(bytes.TrimSuffix(raw, []byte{'\n'}), []byte{'\r'})
		if !bytes.Equal(row.Bytes, raw) {
			t.Fatalf("source coordinates do not recover record at %d", row.Start)
		}
	}
}
