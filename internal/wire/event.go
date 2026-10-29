package wire

import "encoding/json"

// Event is one ordered provider frame in an attachment wire feed.
type Event struct {
	Seq int64           `json:"seq"`
	TS  string          `json:"ts"`
	Dir string          `json:"dir"`
	Msg json.RawMessage `json:"msg"`
}
