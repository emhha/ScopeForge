-- 0007_v2: 覆盖矩阵(coverage_matrix)——全面性核心(docs/phase2/03 §3.1)
-- 格子 = (CWE × Asset × Endpoint) 的一个探索单元,状态机 open → claimed → confirmed|dead|skipped。
-- 写入者:Dispatcher(攻击面落地/accepted 回执)与 Scheduler(认领/穷尽),经 internal/coverage 包。

CREATE TABLE IF NOT EXISTS coverage_matrix (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  challenge_id TEXT NOT NULL,
  cwe          TEXT,                      -- 可空 = 攻击面落地时的"未指定"格
  asset        TEXT NOT NULL,
  endpoint     TEXT,                      -- 可空(资产级格子)
  status       TEXT NOT NULL DEFAULT 'open', -- open|claimed|confirmed|dead|skipped
  form         TEXT,                      -- 端点形态(带参/文件/上传/认证/未知),接力生成依据
  skip_reason  TEXT,                      -- skipped 排除理由(穷尽声明,进报告)
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL
);

-- 格子唯一键:同 (challenge, cwe, asset, endpoint) 只存在一格(NULL 以空串归一)
CREATE UNIQUE INDEX IF NOT EXISTS idx_coverage_key ON coverage_matrix(
  challenge_id, COALESCE(cwe,''), asset, COALESCE(endpoint,'')
);
CREATE INDEX IF NOT EXISTS idx_coverage_status ON coverage_matrix(challenge_id, status);
