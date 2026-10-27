// Package model defines Item and Turn and their supporting source evidence.
// It depends only on the standard library. Provider readers, private assembly
// helpers and external consumers may depend on it; it never imports Nano types.
// History converts the mirrored transcript fields at its own boundary.
//
// SourceSeq is the one-based order in which logical items first appear in one
// source incarnation. Revisions and replays retain that position. A turn's
// StartSourceSeq and EndSourceSeq bound its items in that same sequence.
// Item.TurnOrdinal and Turn.Ordinal count turns within the source. These are
// never Nano projection or delivery ordinals; history assigns those at its
// conversion boundary. Completion observations carry their own source positions.
//
// Observations retain source order beneath an item. Scope separates caller-local
// keys from nullable native identities. NativeParentCallID keeps nested tool
// traffic separate when the provider supplies no child session identity.
// Claude NativeItemID is the API-message-ID:block-index composite. Its
// claude.messageBlock alias records native authority for a provider-supplied
// index and derived (provisional) authority for a counted index. Consumers call
// ExactNativeItemID before strong identity matching. Component aliases retain
// their own basis. Missing API identity stays empty.
// Aliases inherit item scope; relations distinguish results, progress and replay without defining a general graph.
// A relation may reference an evicted observation through native scope and an
// exact scalar alias. Consumers resolve it against persisted evidence; missing
// or ambiguous targets retain the standalone incoming observation and diagnostic.
// This reference never repopulates the reader checkpoint with settled output.
// SerializationTwin is required by Codex paired-first-turn and
// edge-nonadjacent-twins-and-real-repeat: response and event rows are distinct
// native serializations, not repeated delivery of one native event.
//
// Capture, execution outcome and answer finality are independent. Complete says
// only that item content generation ended. A tool block stop is not execution
// success, and a final answer is not a successful turn terminal. Observations and
// completion facts retain the evidence; Status and IsFinalAnswer are compatibility
// summaries, not stronger authority than Outcome and Finality.
//
// Native, capture and caller-observed times stay separate. Null native time stays
// null when the other clocks are known. TimeMeaning enumerates the event's meaning;
// timestamps never establish transcript order.
//
// Usage observations retain response, turn and thread grain, revisions and native
// counter meanings. Turn.Usage names its selected basis and observation refs.
// Missing counters remain nil; totals are never manufactured from other fields.
//
// Metadata adds typed catalog values beneath observations, limited to the
// existing catalog field set. Known, unknown and unsupported stay distinct.
// Activity clocks distinguish source bookkeeping, conversation, and a native
// session document's reported activity; they never imply turn completion.
// Native scope/source/clocks supply provenance; filename fallback and catalog
// reduction belong to consumers. Metadata never defines a Thread unit.
package model
