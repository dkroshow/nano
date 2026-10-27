package history

const schemaV1 = `
CREATE TABLE IF NOT EXISTS schema_migrations (
  version INTEGER PRIMARY KEY,
  applied_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS catalog_state (
  singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
  catalog_version INTEGER NOT NULL
);
INSERT OR IGNORE INTO catalog_state(singleton, catalog_version) VALUES (1, 0);

CREATE TABLE IF NOT EXISTS source_roots (
  id INTEGER PRIMARY KEY,
  provider TEXT NOT NULL,
  root_kind TEXT NOT NULL,
  canonical_root TEXT NOT NULL,
  archived INTEGER NOT NULL DEFAULT 0,
  enabled INTEGER NOT NULL DEFAULT 1,
  UNIQUE(provider, root_kind, canonical_root)
);

CREATE TABLE IF NOT EXISTS source_files (
  id INTEGER PRIMARY KEY,
  source_root_id INTEGER NOT NULL REFERENCES source_roots(id),
  canonical_path TEXT NOT NULL,
  device INTEGER,
  inode INTEGER,
  size INTEGER NOT NULL DEFAULT 0,
  mtime TEXT,
  complete_offset INTEGER NOT NULL DEFAULT 0,
  complete_lines INTEGER NOT NULL DEFAULT 0,
  generation INTEGER NOT NULL DEFAULT 1,
  classification TEXT NOT NULL,
  status TEXT NOT NULL,
  archived INTEGER NOT NULL DEFAULT 0,
  parser_version TEXT NOT NULL DEFAULT '',
  diagnostics_json BLOB NOT NULL DEFAULT '[]',
  diagnostic_summary_json BLOB NOT NULL DEFAULT '{}',
  error_summary TEXT,
  last_indexed_at TEXT,
  UNIQUE(source_root_id, canonical_path)
);

CREATE TABLE IF NOT EXISTS threads (
  id TEXT PRIMARY KEY,
  source_root_id INTEGER NOT NULL REFERENCES source_roots(id),
  provider TEXT NOT NULL,
  native_thread_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  parent_native_id TEXT,
  provider_version TEXT,
  title TEXT,
  preview TEXT,
  created_at TEXT,
  updated_at TEXT,
  metadata_json BLOB NOT NULL,
  archived INTEGER NOT NULL DEFAULT 0,
  source_status TEXT NOT NULL,
  conflict INTEGER NOT NULL DEFAULT 0,
  diagnostic_count INTEGER NOT NULL DEFAULT 0,
  last_indexed_at TEXT,
  UNIQUE(source_root_id, provider, native_thread_id)
);

CREATE TABLE IF NOT EXISTS thread_sources (
  thread_id TEXT NOT NULL REFERENCES threads(id),
  source_file_id INTEGER NOT NULL REFERENCES source_files(id),
  identity_evidence TEXT NOT NULL,
  PRIMARY KEY(thread_id, source_file_id),
  UNIQUE(source_file_id)
);

CREATE TABLE IF NOT EXISTS index_state (
  source_root_id INTEGER PRIMARY KEY REFERENCES source_roots(id),
  state TEXT NOT NULL,
  source_count INTEGER NOT NULL DEFAULT 0,
  last_completed_at TEXT,
  error_summary TEXT
);

CREATE INDEX IF NOT EXISTS threads_catalog_order
  ON threads(updated_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS threads_provider_kind_archived
  ON threads(provider, kind, archived);
CREATE INDEX IF NOT EXISTS threads_native_thread_id
  ON threads(native_thread_id);
CREATE INDEX IF NOT EXISTS source_files_root_status
  ON source_files(source_root_id, status);
`

// schemaV3 is intentionally separate from the metadata tables. Provider
// metadata and transcript projection have independent parser versions and
// checkpoints; changing one must never invalidate the other.
const schemaV3 = `
CREATE TABLE IF NOT EXISTS conversation_projections (
  id INTEGER PRIMARY KEY,
  projection_key TEXT NOT NULL UNIQUE,
  thread_id TEXT NOT NULL REFERENCES threads(id),
  source_file_id INTEGER NOT NULL REFERENCES source_files(id),
  source_generation INTEGER NOT NULL,
  provider TEXT NOT NULL,
  parser_version TEXT NOT NULL,
  completion_rule_version TEXT NOT NULL,
  schema_version INTEGER NOT NULL,
  taxonomy_version TEXT NOT NULL,
  state TEXT NOT NULL,
  complete_offset INTEGER NOT NULL DEFAULT 0,
  complete_lines INTEGER NOT NULL DEFAULT 0,
  parser_state_json BLOB NOT NULL DEFAULT '{}',
  item_count INTEGER NOT NULL DEFAULT 0,
  latest_seq INTEGER NOT NULL DEFAULT 0,
  last_indexed_at TEXT NOT NULL,
  error_summary TEXT
);

CREATE UNIQUE INDEX IF NOT EXISTS conversation_projections_one_active
  ON conversation_projections(thread_id) WHERE state = 'active';
CREATE INDEX IF NOT EXISTS conversation_projections_source
  ON conversation_projections(source_file_id, source_generation);

CREATE TABLE IF NOT EXISTS conversation_turns (
  projection_id INTEGER NOT NULL REFERENCES conversation_projections(id) ON DELETE CASCADE,
  turn_key TEXT NOT NULL,
  native_turn_id TEXT,
  ordinal INTEGER NOT NULL,
  start_seq INTEGER NOT NULL,
  end_seq INTEGER NOT NULL,
  completion_seq INTEGER,
  status TEXT NOT NULL,
  completion_evidence TEXT NOT NULL,
  final_item_id TEXT,
  finality_source TEXT,
  finality_confidence TEXT,
  started_at TEXT,
  completed_at TEXT,
  duration_ms INTEGER,
  config_json BLOB NOT NULL DEFAULT '{}',
  PRIMARY KEY(projection_id, turn_key),
  UNIQUE(projection_id, ordinal)
);

CREATE INDEX IF NOT EXISTS conversation_turns_seq
  ON conversation_turns(projection_id, start_seq, end_seq);

CREATE TABLE IF NOT EXISTS conversation_items (
  projection_id INTEGER NOT NULL REFERENCES conversation_projections(id) ON DELETE CASCADE,
  item_id TEXT NOT NULL,
  projected_seq INTEGER NOT NULL,
  source_key TEXT NOT NULL,
  native_item_id TEXT,
  turn_key TEXT NOT NULL,
  turn_ordinal INTEGER NOT NULL,
  item_type TEXT NOT NULL,
  item_status TEXT NOT NULL,
  role TEXT,
  display_text TEXT NOT NULL DEFAULT '',
  tool_name TEXT,
  call_id TEXT,
  detail_json BLOB NOT NULL DEFAULT '{}',
  created_at TEXT,
  phase TEXT,
  filter_tags_json BLOB NOT NULL DEFAULT '[]',
  synthetic INTEGER NOT NULL DEFAULT 0,
  sidechain INTEGER NOT NULL DEFAULT 0,
  is_final_answer INTEGER NOT NULL DEFAULT 0,
  content_ref TEXT,
  content_truncated INTEGER NOT NULL DEFAULT 0,
  original_bytes INTEGER NOT NULL DEFAULT 0,
  source_line INTEGER NOT NULL,
  source_row_type TEXT NOT NULL,
  source_payload_type TEXT,
  emission_index INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(projection_id, item_id),
  UNIQUE(projection_id, projected_seq),
  UNIQUE(projection_id, source_key),
  FOREIGN KEY(projection_id, turn_key)
    REFERENCES conversation_turns(projection_id, turn_key) DEFERRABLE INITIALLY DEFERRED
);

CREATE INDEX IF NOT EXISTS conversation_items_page
  ON conversation_items(projection_id, projected_seq DESC);

CREATE TABLE IF NOT EXISTS conversation_item_bodies (
  projection_id INTEGER NOT NULL,
  item_id TEXT NOT NULL,
  content_ref TEXT NOT NULL,
  body_text TEXT,
  body_json BLOB,
  original_bytes INTEGER NOT NULL,
  stored_bytes INTEGER NOT NULL,
  truncated INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(projection_id, item_id),
  UNIQUE(content_ref),
  FOREIGN KEY(projection_id, item_id)
    REFERENCES conversation_items(projection_id, item_id) ON DELETE CASCADE
);
`

const schemaV4 = `
ALTER TABLE conversation_items
ADD COLUMN source_line_sha256 TEXT NOT NULL DEFAULT '';
`

// schemaV5 adds the derived interaction class as a stored, filterable column.
// Existing rows default to unclassified; the metadata parser-version bump forces
// a full reprojection that recomputes the true class from the verbatim origin.
const schemaV5 = `
ALTER TABLE threads
ADD COLUMN interaction TEXT NOT NULL DEFAULT 'unclassified';
CREATE INDEX IF NOT EXISTS threads_interaction ON threads(kind, interaction);
`

// schemaV6 adds the derived copy of the user thread-names truth file. It holds
// no source-of-truth data: every row is rebuilt from the flat file on refresh,
// so history.sqlite stays fully delete-and-rebuild safe. Keyed by the durable
// (provider, native_thread_id), never the disposable Nano thread ID.
const schemaV6 = `
CREATE TABLE IF NOT EXISTS thread_names (
  provider TEXT NOT NULL,
  native_thread_id TEXT NOT NULL,
  name TEXT NOT NULL,
  provenance TEXT NOT NULL,
  updated_at TEXT,
  PRIMARY KEY(provider, native_thread_id)
);
`

// schemaV7 adds provider-agnostic per-turn token accounting for analytics.
// Every column is nullable INTEGER: NULL means the provider did not report that
// quantity for the turn. Populated from Codex token_count events and Claude
// assistant message.usage; the transcript parser-version bumps reproject
// existing threads so the columns backfill without a data migration.
const schemaV7 = `
ALTER TABLE conversation_turns ADD COLUMN usage_input INTEGER;
ALTER TABLE conversation_turns ADD COLUMN usage_cached INTEGER;
ALTER TABLE conversation_turns ADD COLUMN usage_output INTEGER;
ALTER TABLE conversation_turns ADD COLUMN usage_reasoning INTEGER;
ALTER TABLE conversation_turns ADD COLUMN usage_total INTEGER;
ALTER TABLE conversation_turns ADD COLUMN context_window INTEGER;
`

// schemaV8 caches Nano-derived current Codex workspace resolutions. The rows
// are disposable: every value can be rebuilt from current Codex configuration
// and the local workspace. Thread-specific recorded settings stay in the
// existing catalog projection and are overlaid at read time.
const schemaV8 = `
CREATE TABLE IF NOT EXISTS codex_workspace_resolutions (
  target_cwd TEXT PRIMARY KEY,
  resolver_version TEXT NOT NULL,
  resolved_at TEXT NOT NULL,
  payload_json BLOB NOT NULL
);
`

// schemaV9 caches Nano-derived current Claude workspace resolutions. Like the
// Codex cache, these rows are disposable and contain only allowlisted,
// non-secret configuration summaries.
const schemaV9 = `
CREATE TABLE IF NOT EXISTS claude_workspace_resolutions (
  target_cwd TEXT PRIMARY KEY,
  resolver_version TEXT NOT NULL,
  resolved_at TEXT NOT NULL,
  payload_json BLOB NOT NULL
);
`

// schemaV10 adds empty, disposable search storage only. Historical document
// discovery and FTS population deliberately happen outside startup migrations.
// The FTS tables are contentless: canonical message text remains owned by the
// normalized conversation item/body tables.
const schemaV10 = `
CREATE TABLE IF NOT EXISTS conversation_search_documents (
  document_id INTEGER PRIMARY KEY,
  projection_id INTEGER NOT NULL,
  item_id TEXT NOT NULL,
  UNIQUE(projection_id, item_id),
  FOREIGN KEY(projection_id, item_id)
    REFERENCES conversation_items(projection_id, item_id) ON DELETE CASCADE
);

CREATE VIRTUAL TABLE IF NOT EXISTS conversation_search_literal_fts
USING fts5(text, content='', contentless_delete=1, tokenize='trigram');

CREATE VIRTUAL TABLE IF NOT EXISTS conversation_search_terms_fts
USING fts5(text, content='', contentless_delete=1, tokenize='unicode61');

CREATE TABLE IF NOT EXISTS conversation_search_state (
  mode TEXT PRIMARY KEY CHECK(mode IN ('literal', 'terms')),
  corpus_rule_version TEXT NOT NULL,
  tokenizer_version TEXT NOT NULL,
  state TEXT NOT NULL CHECK(state IN ('needs_build', 'building', 'ready', 'failed')),
  checkpoint_projection_id INTEGER,
  checkpoint_item_id TEXT,
  checkpoint_document_id INTEGER,
  document_count INTEGER NOT NULL DEFAULT 0 CHECK(document_count >= 0),
  indexed_count INTEGER NOT NULL DEFAULT 0 CHECK(indexed_count >= 0),
  revision INTEGER NOT NULL DEFAULT 0 CHECK(revision >= 0),
  started_at TEXT,
  updated_at TEXT NOT NULL,
  completed_at TEXT,
  error_summary TEXT CHECK(error_summary IS NULL OR length(error_summary) <= 512)
);
`

const schemaV11 = `
ALTER TABLE conversation_turns ADD COLUMN usage_cache_read INTEGER;
ALTER TABLE conversation_turns ADD COLUMN usage_cache_write INTEGER;
ALTER TABLE conversation_turns ADD COLUMN usage_native_json BLOB NOT NULL DEFAULT '{}';
ALTER TABLE conversation_turns ADD COLUMN usage_contract INTEGER;

CREATE VIEW IF NOT EXISTS thread_turn_usage AS
SELECT
  t.id AS thread_id,
  t.provider,
  t.native_thread_id,
  cp.parser_version,
  ct.usage_contract,
  ct.turn_key,
  ct.native_turn_id,
  ct.ordinal AS turn_ordinal,
  ct.started_at,
  ct.completed_at,
  ct.duration_ms,
  CASE WHEN ct.usage_contract = 1 THEN ct.usage_input END AS input_tokens,
  CASE WHEN ct.usage_contract = 1 THEN ct.usage_output END AS output_tokens,
  CASE WHEN ct.usage_contract = 1 THEN ct.usage_reasoning END AS reasoning_tokens,
  CASE WHEN ct.usage_contract = 1 THEN ct.usage_cache_read END AS cache_read_tokens,
  CASE WHEN ct.usage_contract = 1 THEN ct.usage_cache_write END AS cache_write_tokens,
  CASE WHEN ct.usage_contract = 1 THEN ct.usage_total END AS native_total_tokens,
  CASE WHEN ct.usage_contract = 1 THEN ct.context_window END AS context_window,
  CASE WHEN ct.usage_contract = 1 THEN ct.usage_native_json ELSE '{}' END AS native_json
FROM conversation_turns ct
JOIN conversation_projections cp
  ON cp.id = ct.projection_id AND cp.state = 'active'
JOIN threads t ON t.id = cp.thread_id;

CREATE VIEW IF NOT EXISTS thread_turn_usage_messages AS
SELECT
  u.thread_id, u.provider, u.native_thread_id, u.parser_version, u.usage_contract,
  u.turn_key, u.native_turn_id, u.turn_ordinal, u.started_at, u.completed_at,
  u.duration_ms, u.input_tokens, u.output_tokens, u.reasoning_tokens,
  u.cache_read_tokens, u.cache_write_tokens, u.native_total_tokens,
  u.context_window, u.native_json,
  ci.item_id, ci.item_type, ci.role, ci.display_text, ci.created_at, ci.projected_seq
FROM thread_turn_usage u
JOIN conversation_projections cp
  ON cp.thread_id = u.thread_id AND cp.state = 'active'
JOIN conversation_items ci
  ON ci.projection_id = cp.id AND ci.turn_key = u.turn_key
WHERE ci.item_type IN ('user_message', 'agent_message')
  AND (ci.sidechain = 0 OR EXISTS (
        SELECT 1 FROM threads th
        WHERE th.id = u.thread_id AND th.kind = 'subagent'
      ));
`

// schemaV12 adds per-turn model and effort onto the usage contract.
// Views are dropped and recreated so consumers can read model/effort without
// joining config_json. Grok is filled later by grok-transcript-v6 from the
// session summary snapshot; see docs/decisions/thread-turn-usage.md.
const schemaV12 = `
ALTER TABLE conversation_turns ADD COLUMN usage_model TEXT;
ALTER TABLE conversation_turns ADD COLUMN usage_effort TEXT;
DROP VIEW IF EXISTS thread_turn_usage_messages;
DROP VIEW IF EXISTS thread_turn_usage;
CREATE VIEW thread_turn_usage AS
SELECT
  t.id AS thread_id,
  t.provider,
  t.native_thread_id,
  cp.parser_version,
  ct.usage_contract,
  ct.turn_key,
  ct.native_turn_id,
  ct.ordinal AS turn_ordinal,
  ct.started_at,
  ct.completed_at,
  ct.duration_ms,
  CASE WHEN ct.usage_contract = 1 THEN ct.usage_input END AS input_tokens,
  CASE WHEN ct.usage_contract = 1 THEN ct.usage_output END AS output_tokens,
  CASE WHEN ct.usage_contract = 1 THEN ct.usage_reasoning END AS reasoning_tokens,
  CASE WHEN ct.usage_contract = 1 THEN ct.usage_cache_read END AS cache_read_tokens,
  CASE WHEN ct.usage_contract = 1 THEN ct.usage_cache_write END AS cache_write_tokens,
  CASE WHEN ct.usage_contract = 1 THEN ct.usage_total END AS native_total_tokens,
  CASE WHEN ct.usage_contract = 1 THEN ct.context_window END AS context_window,
  CASE WHEN ct.usage_contract = 1 THEN ct.usage_model END AS model,
  CASE WHEN ct.usage_contract = 1 THEN ct.usage_effort END AS effort,
  CASE WHEN ct.usage_contract = 1 THEN ct.usage_native_json ELSE '{}' END AS native_json
FROM conversation_turns ct
JOIN conversation_projections cp
  ON cp.id = ct.projection_id AND cp.state = 'active'
JOIN threads t ON t.id = cp.thread_id;

CREATE VIEW thread_turn_usage_messages AS
SELECT
  u.thread_id, u.provider, u.native_thread_id, u.parser_version, u.usage_contract,
  u.turn_key, u.native_turn_id, u.turn_ordinal, u.started_at, u.completed_at,
  u.duration_ms, u.input_tokens, u.output_tokens, u.reasoning_tokens,
  u.cache_read_tokens, u.cache_write_tokens, u.native_total_tokens,
  u.context_window, u.model, u.effort, u.native_json,
  ci.item_id, ci.item_type, ci.role, ci.display_text, ci.created_at, ci.projected_seq
FROM thread_turn_usage u
JOIN conversation_projections cp
  ON cp.thread_id = u.thread_id AND cp.state = 'active'
JOIN conversation_items ci
  ON ci.projection_id = cp.id AND ci.turn_key = u.turn_key
WHERE ci.item_type IN ('user_message', 'agent_message')
  AND (ci.sidechain = 0 OR EXISTS (
        SELECT 1 FROM threads th
        WHERE th.id = u.thread_id AND th.kind = 'subagent'
      ));
`

// schemaV13 adds per-turn Codex service tier. Its views are recreated because
// service_tier is part of the supported per-turn usage contract.
const schemaV13 = `
ALTER TABLE conversation_turns ADD COLUMN usage_service_tier TEXT;
DROP VIEW IF EXISTS thread_turn_usage_messages;
DROP VIEW IF EXISTS thread_turn_usage;
CREATE VIEW thread_turn_usage AS
SELECT
  t.id AS thread_id,
  t.provider,
  t.native_thread_id,
  cp.parser_version,
  ct.usage_contract,
  ct.turn_key,
  ct.native_turn_id,
  ct.ordinal AS turn_ordinal,
  ct.started_at,
  ct.completed_at,
  ct.duration_ms,
  CASE WHEN ct.usage_contract = 1 THEN ct.usage_input END AS input_tokens,
  CASE WHEN ct.usage_contract = 1 THEN ct.usage_output END AS output_tokens,
  CASE WHEN ct.usage_contract = 1 THEN ct.usage_reasoning END AS reasoning_tokens,
  CASE WHEN ct.usage_contract = 1 THEN ct.usage_cache_read END AS cache_read_tokens,
  CASE WHEN ct.usage_contract = 1 THEN ct.usage_cache_write END AS cache_write_tokens,
  CASE WHEN ct.usage_contract = 1 THEN ct.usage_total END AS native_total_tokens,
  CASE WHEN ct.usage_contract = 1 THEN ct.context_window END AS context_window,
  CASE WHEN ct.usage_contract = 1 THEN ct.usage_model END AS model,
  CASE WHEN ct.usage_contract = 1 THEN ct.usage_effort END AS effort,
  CASE WHEN ct.usage_contract = 1 THEN ct.usage_service_tier END AS service_tier,
  CASE WHEN ct.usage_contract = 1 THEN ct.usage_native_json ELSE '{}' END AS native_json
FROM conversation_turns ct
JOIN conversation_projections cp
  ON cp.id = ct.projection_id AND cp.state = 'active'
JOIN threads t ON t.id = cp.thread_id;

CREATE VIEW thread_turn_usage_messages AS
SELECT
  u.thread_id, u.provider, u.native_thread_id, u.parser_version, u.usage_contract,
  u.turn_key, u.native_turn_id, u.turn_ordinal, u.started_at, u.completed_at,
  u.duration_ms, u.input_tokens, u.output_tokens, u.reasoning_tokens,
  u.cache_read_tokens, u.cache_write_tokens, u.native_total_tokens,
  u.context_window, u.model, u.effort, u.service_tier, u.native_json,
  ci.item_id, ci.item_type, ci.role, ci.display_text, ci.created_at, ci.projected_seq
FROM thread_turn_usage u
JOIN conversation_projections cp
  ON cp.thread_id = u.thread_id AND cp.state = 'active'
JOIN conversation_items ci
  ON ci.projection_id = cp.id AND ci.turn_key = u.turn_key
WHERE ci.item_type IN ('user_message', 'agent_message')
  AND (ci.sidechain = 0 OR EXISTS (
        SELECT 1 FROM threads th
        WHERE th.id = u.thread_id AND th.kind = 'subagent'
      ));
`

// schemaV14 preserves pre-canonical projection item ids as compatibility
// aliases. The map is Nano-owned, durable across restarts, and intentionally
// has no retirement path in this migration: removal needs separately reviewed
// live-consumer evidence.
const schemaV14 = `
CREATE TABLE IF NOT EXISTS conversation_item_aliases (
  thread_id TEXT NOT NULL REFERENCES threads(id),
  old_item_id TEXT NOT NULL,
  canonical_item_id TEXT NOT NULL,
  source_key TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY(thread_id, old_item_id),
  UNIQUE(thread_id, source_key, canonical_item_id)
);
CREATE INDEX IF NOT EXISTS conversation_item_aliases_canonical
  ON conversation_item_aliases(thread_id, canonical_item_id);
`

// schemaV15 replaces the old global uniqueness constraint on content_ref.
// Canonical item ids intentionally produce the same content reference across
// retained projections, while evidence rows remain distinguished by their
// projection_id/item_id primary key.
const schemaV15 = `
CREATE TABLE conversation_item_bodies_v15 (
  projection_id INTEGER NOT NULL,
  item_id TEXT NOT NULL,
  content_ref TEXT NOT NULL,
  body_text TEXT,
  body_json BLOB,
  original_bytes INTEGER NOT NULL,
  stored_bytes INTEGER NOT NULL,
  truncated INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(projection_id, item_id),
  FOREIGN KEY(projection_id, item_id)
    REFERENCES conversation_items(projection_id, item_id) ON DELETE CASCADE
);
INSERT INTO conversation_item_bodies_v15(
  projection_id, item_id, content_ref, body_text, body_json, original_bytes,
  stored_bytes, truncated)
SELECT projection_id, item_id, content_ref, body_text, body_json, original_bytes,
       stored_bytes, truncated
FROM conversation_item_bodies;
DROP TABLE conversation_item_bodies;
ALTER TABLE conversation_item_bodies_v15 RENAME TO conversation_item_bodies;
CREATE INDEX conversation_item_bodies_content_ref
  ON conversation_item_bodies(content_ref);
`

// schemaV16 records a legacy alias collision without failing the entire
// replacement projection. Such a conflict means an old projection-scoped id
// was already associated with a different canonical item. The active
// projection remains authoritative; the durable row makes the disagreement
// inspectable while preserving every non-conflicting alias.
const schemaV16 = `
CREATE TABLE IF NOT EXISTS conversation_item_alias_conflicts (
  thread_id TEXT NOT NULL REFERENCES threads(id),
  old_item_id TEXT NOT NULL,
  existing_canonical_item_id TEXT NOT NULL,
  candidate_canonical_item_id TEXT NOT NULL,
  source_key TEXT NOT NULL,
  detected_at TEXT NOT NULL,
  PRIMARY KEY(thread_id, old_item_id, candidate_canonical_item_id)
);
CREATE INDEX IF NOT EXISTS conversation_item_alias_conflicts_thread
  ON conversation_item_alias_conflicts(thread_id, detected_at DESC);
`

// Receipts belong to projection generations, not reader checkpoints. The
// leading lookup columns restrict every identity query to an incoming key.
const schemaV17 = `
CREATE TABLE IF NOT EXISTS conversation_parse_values (
  id INTEGER PRIMARY KEY,
  projection_id INTEGER NOT NULL REFERENCES conversation_projections(id) ON DELETE CASCADE,
  value_key BLOB NOT NULL,
  UNIQUE(projection_id, value_key)
);
CREATE TABLE IF NOT EXISTS conversation_parse_identities (
  id INTEGER PRIMARY KEY,
  projection_id INTEGER NOT NULL REFERENCES conversation_projections(id) ON DELETE CASCADE,
  identity_kind TEXT NOT NULL,
  identity_value_id INTEGER NOT NULL REFERENCES conversation_parse_values(id),
  scope_id INTEGER NOT NULL REFERENCES conversation_parse_values(id),
  item_id TEXT NOT NULL,
  native_item_value_id INTEGER REFERENCES conversation_parse_values(id),
  call_value_id INTEGER REFERENCES conversation_parse_values(id),
  item_type TEXT NOT NULL,
  role TEXT NOT NULL,
  observation_value_id INTEGER REFERENCES conversation_parse_values(id),
  native_event_digest TEXT NOT NULL,
  UNIQUE(projection_id, identity_kind, identity_value_id, scope_id, item_id)
);
CREATE TABLE IF NOT EXISTS conversation_observation_aliases (
  projection_id INTEGER NOT NULL REFERENCES conversation_projections(id) ON DELETE CASCADE,
  namespace TEXT NOT NULL,
  value_id INTEGER NOT NULL REFERENCES conversation_parse_values(id),
  observation_value_id INTEGER NOT NULL REFERENCES conversation_parse_values(id),
  scope_id INTEGER NOT NULL REFERENCES conversation_parse_values(id),
  PRIMARY KEY(projection_id, namespace, value_id, observation_value_id, scope_id)
);
`

// Ordered evidence stores source addresses and scalar facts. Source payloads
// are read on demand, never copied into observation or item snapshots.
const schemaV18 = `
CREATE TABLE conversation_observations (
  id INTEGER PRIMARY KEY,
  projection_id INTEGER NOT NULL REFERENCES conversation_projections(id) ON DELETE CASCADE,
  key_id INTEGER NOT NULL REFERENCES conversation_parse_values(id),
  scope_id INTEGER NOT NULL REFERENCES conversation_parse_values(id),
  source_id INTEGER NOT NULL REFERENCES conversation_parse_values(id),
  block_path_id INTEGER NOT NULL REFERENCES conversation_parse_values(id),
  source_line INTEGER NOT NULL,
  source_frame INTEGER NOT NULL,
  byte_start INTEGER NOT NULL,
  byte_length INTEGER NOT NULL,
  source_sha256 BLOB NOT NULL,
  facts_blob BLOB NOT NULL,
  sha256 BLOB NOT NULL,
  UNIQUE(projection_id, key_id)
);
CREATE TABLE conversation_item_observations (
  projection_id INTEGER NOT NULL,
  item_id TEXT NOT NULL,
  observation_id INTEGER NOT NULL REFERENCES conversation_observations(id) ON DELETE CASCADE,
  PRIMARY KEY(projection_id, item_id, observation_id),
  FOREIGN KEY(projection_id, item_id) REFERENCES conversation_items(projection_id, item_id) ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
  FOREIGN KEY(projection_id) REFERENCES conversation_projections(id) ON DELETE CASCADE
);
CREATE TABLE conversation_turn_observations (
  projection_id INTEGER NOT NULL,
  turn_key TEXT NOT NULL,
  observation_id INTEGER NOT NULL REFERENCES conversation_observations(id) ON DELETE CASCADE,
  PRIMARY KEY(projection_id, turn_key, observation_id),
  FOREIGN KEY(projection_id, turn_key) REFERENCES conversation_turns(projection_id, turn_key) ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
  FOREIGN KEY(projection_id) REFERENCES conversation_projections(id) ON DELETE CASCADE
);
CREATE TABLE conversation_relations (
  projection_id INTEGER NOT NULL REFERENCES conversation_projections(id) ON DELETE CASCADE,
  relation_key BLOB NOT NULL,
  relation_kind TEXT NOT NULL,
  from_item_id TEXT NOT NULL,
  to_item_id TEXT NOT NULL,
  from_observation_value_id INTEGER REFERENCES conversation_parse_values(id),
  to_observation_value_id INTEGER REFERENCES conversation_parse_values(id),
  relation_json BLOB NOT NULL,
  PRIMARY KEY(projection_id, relation_key)
);
CREATE INDEX conversation_relations_from_item ON conversation_relations(projection_id, from_item_id);
CREATE INDEX conversation_relations_to_item ON conversation_relations(projection_id, to_item_id);
`

const schemaV20 = `
ALTER TABLE conversation_turns ADD COLUMN parsed_json BLOB NOT NULL DEFAULT '{}';
CREATE TABLE conversation_turn_aliases (
  projection_id INTEGER NOT NULL,
  local_value_id INTEGER NOT NULL REFERENCES conversation_parse_values(id),
  scope_id INTEGER NOT NULL REFERENCES conversation_parse_values(id),
  turn_key TEXT NOT NULL,
  PRIMARY KEY(projection_id, local_value_id, scope_id),
  FOREIGN KEY(projection_id, turn_key) REFERENCES conversation_turns(projection_id, turn_key) ON DELETE CASCADE
);
CREATE INDEX conversation_turns_native ON conversation_turns(projection_id, native_turn_id);
CREATE INDEX conversation_items_source_line ON conversation_items(projection_id, source_line, emission_index);
`

// Superseded collection asks for retained generations by state, thread and
// descending generation id. Without this covering index, a recovery pass
// rescans the complete projection table once per rebuilt thread.
// conversation_items already has several projection-leading indexes,
// including conversation_items_page, so no redundant single-column index is
// needed there.
const schemaV23 = `
CREATE INDEX IF NOT EXISTS conversation_projections_state_thread_id
ON conversation_projections(state, thread_id, id DESC);
`

// Writing a parsed turn looks up its items by (projection_id, turn_key): its
// sequence range and its final-answer flags. With only projection-leading
// indexes each lookup scanned every item in the projection, so rebuilding a
// transcript cost turns times items, and a large rebuild held the catalog's
// projection lock for minutes.
const schemaV26 = `
CREATE INDEX IF NOT EXISTS conversation_items_turn
ON conversation_items(projection_id, turn_key);
`

// transcriptMessageSQL is the one definition of a message row, the only rows
// Enhanced shows: a user or agent message, or a question and its answer, in
// the main thread, whatever its status. It names row types, like the live
// message feed, so a saved row exists for every message the feed showed.
// Equalities joined by OR, not an IN list: SQLite checks the predicate on
// every item written, and the IN form's added write cost measured several
// times higher.
const transcriptMessageSQL = `(item_type = 'user_message' OR item_type = 'agent_message' OR item_type = 'interaction') AND sidechain = 0`

// Enhanced pages walk back from the newest row to find 40 messages. With only
// conversation_items_page that walk classified every hidden reasoning and tool
// row on the way, often hundreds per page. SQLite uses a partial index only
// for a query whose WHERE repeats its predicate, and a created index keeps
// the predicate text it was created with: changing transcriptMessageSQL needs
// a new migration.
const schemaV27 = `
CREATE INDEX IF NOT EXISTS conversation_items_messages_page
ON conversation_items(projection_id, projected_seq DESC) WHERE ` + transcriptMessageSQL + `;
`

const schemaV24 = `
CREATE TABLE IF NOT EXISTS conversation_parser_state_entries (
  projection_id INTEGER NOT NULL REFERENCES conversation_projections(id) ON DELETE CASCADE,
  entry_sequence INTEGER NOT NULL,
  field TEXT NOT NULL,
  entry_key TEXT NOT NULL,
  entry_json BLOB NOT NULL,
  entry_sha256 BLOB NOT NULL,
  PRIMARY KEY(projection_id, entry_sequence),
  UNIQUE(projection_id, field, entry_key)
);
`

// schemaV25 records a narrow parser-upgrade inspection without pretending the
// old checkpoint is compatible with the new reader. The source snapshot is
// part of the key: an append or rewrite naturally makes the decision stale.
// These rows are disposable derived state, just like the projections they
// qualify.
const schemaV25 = `
CREATE TABLE IF NOT EXISTS conversation_parser_checks (
  source_file_id INTEGER NOT NULL REFERENCES source_files(id) ON DELETE CASCADE,
  source_generation INTEGER NOT NULL,
  source_size INTEGER NOT NULL,
  source_mtime TEXT NOT NULL,
  from_parser_version TEXT NOT NULL,
  to_parser_version TEXT NOT NULL,
  from_completion_rule_version TEXT NOT NULL,
  to_completion_rule_version TEXT NOT NULL,
  outcome TEXT NOT NULL CHECK(outcome IN ('unaffected')),
  checked_at TEXT NOT NULL,
  PRIMARY KEY(source_file_id, source_generation, source_size, source_mtime,
              from_parser_version, to_parser_version,
              from_completion_rule_version, to_completion_rule_version)
);
`
