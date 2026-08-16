-- 0004_m4: benchmark 运行记录(bench_runs 表,docs/06 §1.2 断点续跑)。
-- 每题结果落库:中断后 --resume 跳过已完成;指标含成功率/耗时/成本/分档。

CREATE TABLE IF NOT EXISTS bench_runs (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id        TEXT NOT NULL,          -- 一次 bench 执行 ID(如 20260805-1)
  set_name      TEXT NOT NULL,          -- ci20 | full100 | 自定义
  challenge_id  TEXT NOT NULL,
  difficulty    TEXT NOT NULL,          -- easy | medium | hard
  dimension     TEXT NOT NULL,          -- web | pivot | binary | cloud | evasion | auth
  status        TEXT NOT NULL,          -- success | fail | skipped | error
  attempts      INTEGER DEFAULT 0,
  correct       INTEGER DEFAULT 0,      -- 1 = 正确提交
  turns         INTEGER DEFAULT 0,
  cost_usd      REAL DEFAULT 0,
  started_at    INTEGER NOT NULL,
  finished_at   INTEGER,
  duration_ms   INTEGER DEFAULT 0,
  error         TEXT,                   -- 错误摘要(失败/异常)
  report_path   TEXT,                   -- 复盘报告路径
  UNIQUE(run_id, challenge_id)
);

CREATE INDEX IF NOT EXISTS idx_bench_runs_run ON bench_runs(run_id);
CREATE INDEX IF NOT EXISTS idx_bench_runs_set ON bench_runs(set_name);
