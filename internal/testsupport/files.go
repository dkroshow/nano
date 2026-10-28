// Package testsupport provides shared synthetic test setup.
package testsupport

import (
	"os"
	"testing"
)

func WriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}
