-- 0001_init: M0 核心表(会话 / 事件流 / 账本 / 子代理转录)
-- 设计契约见 docs/01 §4。黑板事实表(facts/intents/leases/attempts)为 M1 追加。

CREATE TABLE IF NOT EXISTS sessions (
  id              TEXT PRIMARY KEY,          -- 会话 ID(UUID)
  kind            TEXT NOT NULL,             -- main | worker | subagent | observer
  challenge_id    TEXT,
  provider        TEXT,
  model           TEXT,
  messages        BLOB NOT NULL,             -- 序列化消息序列(含工具调用配对)
  rewrite_version INTEGER DEFAULT 0,         -- 压缩/重写版本
  digest          TEXT,                      -- 内容摘要,防并发写
  metadata        BLOB,                      -- 会话元数据(JSON)
  branch          TEXT NOT NULL DEFAULT 'main', -- snapshot | rewrite | recovery 分支标记
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_sessions_kind ON sessions(kind);
CREATE INDEX IF NOT EXISTS idx_sessions_updated ON sessions(updated_at);

-- 事件流:审计/回放/报告的唯一事实源(SSE 按 seq 增量推送)
CREATE TABLE IF NOT EXISTS events (
  seq          INTEGER PRIMARY KEY AUTOINCREMENT,
  ts           INTEGER NOT NULL,
  kind         TEXT NOT NULL,                -- turn_start | reasoning_delta | text_delta |
                                             -- tool_call_start | tool_call_args_delta |
                                             -- tool_call_result | usage | submission | finding |
                                             -- steer | checkpoint | error
  payload      BLOB,                         -- JSON 载荷
  session_id   TEXT,
  challenge_id TEXT
);

CREATE INDEX IF NOT EXISTS idx_events_session ON events(session_id, seq);
CREATE INDEX IF NOT EXISTS idx_events_ts ON events(ts);

-- 账本:成本熔断的数据源(M1 启用)
CREATE TABLE IF NOT EXISTS ledger (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id       TEXT,
  role             TEXT,
  model            TEXT,
  prompt_tokens    INTEGER DEFAULT 0,
  cache_hit_tokens INTEGER DEFAULT 0,
  output_tokens    INTEGER DEFAULT 0,
  reasoning_tokens INTEGER DEFAULT 0,
  cost_usd         REAL DEFAULT 0,
  ts               INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_ledger_session ON ledger(session_id);

-- 子代理转录(M0 迁移)
CREATE TABLE IF NOT EXISTS subagent_transcripts (
  id          TEXT PRIMARY KEY,
  parent_id   TEXT,
  depth       INTEGER DEFAULT 0,
  prompt      TEXT,
  result      TEXT,
  status      TEXT NOT NULL DEFAULT 'running', -- running | completed | failed
  model       TEXT,
  created_at  INTEGER NOT NULL,
  completed_at INTEGER
);

CREATE INDEX IF NOT EXISTS idx_subagent_parent ON subagent_transcripts(parent_id);
