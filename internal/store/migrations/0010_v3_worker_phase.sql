-- 0010_v3_worker_phase: workers 表增加 phase 列
-- Phase 3(07 合并): Worker 类型合并为 Operator + Synthesizer,
-- Operator 的运行阶段(recon/explore/reason)由 phase 列区分。

ALTER TABLE workers ADD COLUMN phase TEXT DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_workers_phase ON workers(phase);
