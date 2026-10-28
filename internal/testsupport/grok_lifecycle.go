package testsupport

import "embed"

//go:embed testdata/grok-lifecycle/*.json
var grokLifecycle embed.FS

// GrokLifecycleFixture reads one captured load or new response.
func GrokLifecycleFixture(name string) ([]byte, error) {
	return grokLifecycle.ReadFile("testdata/grok-lifecycle/" + name)
}
