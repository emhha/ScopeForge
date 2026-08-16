-- 0003_m1: sessions 表复合主键 (id, branch),使多分支共存
-- M0 的 sessions.id 是主键,SaveRecoveryBranch 的 INSERT ON CONFLICT(id) 会覆盖
-- 快照/重写/恢复行,恢复分支从未真正独立保存(docs/03 §7 会话损坏回滚的前提)。

CREATE TABLE IF NOT EXISTS sessions_new (
  id              TEXT NOT NULL,
  kind            TEXT NOT NULL,
  challenge_id    TEXT,
  provider        TEXT,
  model           TEXT,
  messages        BLOB NOT NULL,
  rewrite_version INTEGER DEFAULT 0,
  digest          TEXT,
  metadata        BLOB,
  branch          TEXT NOT NULL DEFAULT 'main', -- snapshot | rewrite | recovery 分支标记
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL,
  PRIMARY KEY (id, branch)
);

INSERT INTO sessions_new SELECT * FROM sessions;

DROP TABLE sessions;
ALTER TABLE sessions_new RENAME TO sessions;

CREATE INDEX IF NOT EXISTS idx_sessions_kind ON sessions(kind);
CREATE INDEX IF NOT EXISTS idx_sessions_updated ON sessions(updated_at);
