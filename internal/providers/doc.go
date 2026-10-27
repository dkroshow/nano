// Package providers defines the five turn operations and declared capabilities
// shared by provider adapters. Adapters take values and return normalized
// lifecycle/parser evidence; they never receive an app or session pointer.
//
// Dependencies point to lifecycle, turns and the parsing model. Concrete
// adapters live in codex, claude and grok subpackages. App may call this contract;
// parsing, transcript identity, restore and history append are not turn verbs.
// Feature read surfaces use declarations without adding protocol dispatch here.
//
// Runtime operation factories bind native clients behind these adapters. Shared
// submit, interrupt, and interaction paths call the adapter contract; start and
// resume remain with the attachment coordinator because they create or attach
// the native transport. Provider protocol translation stays at that edge.
package providers
