// Package transcriptparse contains deterministic readers of provider transcript bytes. Provider
// packages and model form its public surface; Nano ingestion and the dump
// command may depend on them. The module depends only on itself and the Go
// standard library.
package transcriptparse
