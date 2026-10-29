// Package protocol preserves the existing root boundary while Codex wire ownership moves.
package protocol

import (
	codex "github.com/dkroshow/nano/internal/wire/codex"
)

const (
	Send = codex.Send
	Recv = codex.Recv
)
