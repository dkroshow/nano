// Package protocol preserves the existing root boundary while Codex wire ownership moves.
package protocol

import codex "github.com/dkroshow/nano/internal/wire/codex"

type Direction = codex.Direction
type RPCError = codex.RPCError

const (
	Send = codex.Send
	Recv = codex.Recv
)
