// Package prompts owns durable prompt delivery through provider acceptance.
// It writes queued_turns and send_attempts. Accepted rows retain the exact
// provider turn id so restart never resends them. Uncertain rows are visible
// but never retried automatically.
package prompts
