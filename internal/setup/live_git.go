package setup

import "github.com/dkroshow/nano/internal/inspection"

// LiveGit is the shared read-only workspace Git cache.
var LiveGit = inspection.NewGitCache()
