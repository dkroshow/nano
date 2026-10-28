package testsupport

// StoredClaudePosture is a remembered session's argv including a stale `--resume`
// pair from an earlier fallback, so every fallback assertion also proves the
// stripping rule.
var StoredClaudePosture = []string{
	"-p", "--verbose", "--resume", "native-0",
	"--permission-mode", "bypassPermissions",
}

const GrokThread = "f3fa6aa0-a137-5782-bc95-1dd0f1ea92b1"
