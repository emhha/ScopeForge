-- 0002_m1: M1 黑板表(facts/intents/leases/attempts/workers + 全局 seq)
-- 设计契约见 docs/01 §4 与 docs/03 §1。写入唯一通道: Dispatcher。

-- 账本补充 challenge 维度(M1 BudgetMeter 熔断数据源)
ALTER TABLE ledger ADD COLUMN challenge_id TEXT;

-- 黑板全局序号:任何黑板变更(事实/意图/尝试)seq+1,
-- Worker 快照携带 asOfSeq,Dispatcher 据此检测写冲突。
CREATE TABLE IF NOT EXISTS blackboard_meta (
  key   TEXT PRIMARY KEY,
  value INTEGER NOT NULL
);

INSERT OR IGNORE INTO blackboard_meta (key, value) VALUES ('seq', 1);

-- 黑板事实(只增;update/delete 以 superseded_by 留血缘,时间旅行)
CREATE TABLE IF NOT EXISTS facts (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  seq           INTEGER NOT NULL,
  prefix        TEXT NOT NULL,              -- obs: | vuln: | flag: | hint: | dead: | hyp:
  text          TEXT NOT NULL,
  weight        REAL DEFAULT 0.5,           -- 置信度 [0,1]
  state         TEXT NOT NULL DEFAULT 'candidate', -- candidate|confirmed|superseded|falsified
  superseded_by INTEGER,                    -- 后继事实 id(NULL=现行)
  created_by    TEXT,                       -- 写入者(worker/observer id)
  evidence_ref  TEXT,                       -- 工具调用 id
  challenge_id  TEXT NOT NULL,
  created_at    INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_facts_challenge ON facts(challenge_id, state, weight DESC);
CREATE INDEX IF NOT EXISTS idx_facts_superseded ON facts(superseded_by);

-- 意图:带权重的探索方向(open → claimed → pending → done/dead)
CREATE TABLE IF NOT EXISTS intents (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  seq          INTEGER NOT NULL,
  challenge_id TEXT NOT NULL,
  text         TEXT NOT NULL,
  weight       REAL DEFAULT 0.5,
  state        TEXT NOT NULL DEFAULT 'open', -- open|claimed|pending|done|dead
  claimed_by   TEXT,
  claimed_at   INTEGER,
  created_by   TEXT,
  created_at   INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_intents_challenge ON intents(challenge_id, state, weight DESC);

-- 租约:互斥(reason 读图 / 提交 flag)
CREATE TABLE IF NOT EXISTS leases (
  resource   TEXT PRIMARY KEY,
  holder     TEXT NOT NULL,
  expires_at INTEGER NOT NULL,
  created_at INTEGER NOT NULL
);

-- 尝试/提交:终止判定第二路的事实来源
CREATE TABLE IF NOT EXISTS attempts (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  challenge_id TEXT NOT NULL,
  flag         TEXT NOT NULL,
  result       TEXT NOT NULL,               -- correct|wrong|rate_limited
  submitted_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_attempts_challenge ON attempts(challenge_id, submitted_at DESC);

-- Worker 状态(调度内核的持久化心跳;resume 时据此恢复态势)
CREATE TABLE IF NOT EXISTS workers (
  id                     TEXT PRIMARY KEY,
  challenge_id           TEXT NOT NULL,
  worker_type            TEXT NOT NULL,     -- bootstrap|explore|reason|conclude
  provider               TEXT,
  model                  TEXT,
  session_id             TEXT,
  status                 TEXT NOT NULL DEFAULT 'running', -- running|done|aborted|failed|orphaned
  handoff                TEXT,
  intent_id              INTEGER,
  last_progress_at       INTEGER NOT NULL,
  has_correct_submission INTEGER NOT NULL DEFAULT 0,
  created_at             INTEGER NOT NULL,
  finished_at            INTEGER
);

CREATE INDEX IF NOT EXISTS idx_workers_challenge ON workers(challenge_id, status);
