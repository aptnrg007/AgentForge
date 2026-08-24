CREATE TABLE IF NOT EXISTS schema_version (
  version INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS agents (
  name       TEXT PRIMARY KEY,
  yaml       TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS runs (
  id           TEXT PRIMARY KEY,
  agent_name   TEXT NOT NULL REFERENCES agents(name),
  state        TEXT NOT NULL,
  turn_count   INTEGER NOT NULL DEFAULT 0,
  repair_count INTEGER NOT NULL DEFAULT 0,
  error        TEXT,
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS messages (
  run_id        TEXT NOT NULL REFERENCES runs(id),
  seq           INTEGER NOT NULL,
  role          TEXT NOT NULL,
  content_json  TEXT NOT NULL,
  created_at    INTEGER NOT NULL,
  -- input_tokens/output_tokens/latency_ms are only ever set on the
  -- assistant message a model call produced (provider.Usage plus how
  -- long the call took); every other role's row keeps them at 0 — see
  -- Store.AppendMessageWithUsage.
  input_tokens  INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  latency_ms    INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (run_id, seq)
);

CREATE TABLE IF NOT EXISTS tool_calls (
  id          TEXT PRIMARY KEY,
  run_id      TEXT NOT NULL REFERENCES runs(id),
  tool_name   TEXT NOT NULL,
  args_json   TEXT NOT NULL,
  approval    TEXT NOT NULL,
  decided_by  TEXT,
  reason      TEXT,
  result_json TEXT,
  is_error    INTEGER NOT NULL DEFAULT 0,
  created_at  INTEGER NOT NULL,
  decided_at  INTEGER,
  executed_at INTEGER,
  -- duration_ms is how long the call itself took to execute (0 for a
  -- denied call, or one that never got an approval decision at all) —
  -- see Store.UpdateToolCallResult.
  duration_ms INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_runs_agent ON runs(agent_name, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_toolcalls_run ON tool_calls(run_id, created_at);
