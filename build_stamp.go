package main

import (
	"encoding/json"
	turns "github.com/dkroshow/nano/internal/core/prompts"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"sync"
)

// buildStamp identifies the running binary. Set it at link time with
// -ldflags "-X main.buildStamp=..." and read it through /api/v1/build.
var buildStamp = "dev"

var embeddedBundleOnce struct {
	sync.Once
	name string
}

// embeddedBundleName is the web/dist entry script compiled into this binary.
// Comparing it against the filename in web/dist proves the running server is
// serving the bundle that was just built, not a stale embed.
func embeddedBundleName() string {
	embeddedBundleOnce.Do(func() {
		entries, err := fs.ReadDir(distFS, "web/dist/assets")
		if err != nil {
			return
		}
		for _, entry := range entries {
			name := entry.Name()
			if strings.HasPrefix(name, "index-") && path.Ext(name) == ".js" {
				embeddedBundleOnce.name = name
				return
			}
		}
	})
	return embeddedBundleOnce.name
}

func handleBuildInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"stamp":       buildStamp,
		"bundle":      embeddedBundleName(),
		"storeSchema": turns.TurnSchemaVersion,
	})
}
