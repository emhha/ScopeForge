-- 0005_v2: 漏洞账本(vulnerability_ledger)——第二阶段终止/评测/报告的核心数据源
-- 设计契约见 docs/phase2/02 §1。一次漏洞提交(ReportVulnerability)产生一条账本记录,
-- 只增不改:记录不删除不覆盖,回执状态通过 status 推进(submitted → accepted/duplicate/false_positive/rejected)。

CREATE TABLE IF NOT EXISTS vulnerability_ledger (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  seq           INTEGER NOT NULL,          -- 黑板全局序号(与 facts/intents 同轨)
  challenge_id  TEXT NOT NULL,             -- 任务 id(复用 challenge_id 列语义)
  cwe           TEXT,                      -- CWE 编号,可空(未归类)
  asset         TEXT NOT NULL,             -- 资产(域名/IP)
  endpoint      TEXT,                      -- 端点/路径,可空
  severity      TEXT NOT NULL DEFAULT 'info', -- critical|high|medium|low|info
  title         TEXT NOT NULL,             -- 漏洞标题(自由文本,给报告用)
  description   TEXT,                      -- 细节描述
  evidence_ref  TEXT,                      -- 证据引用(工具调用 id)
  platform_ref  TEXT,                      -- 平台回执 id(接受/重复/误报)
  status        TEXT NOT NULL DEFAULT 'submitted', -- submitted|accepted|duplicate|false_positive|rejected
  submitted_at  INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_ledger_challenge ON vulnerability_ledger(challenge_id, submitted_at DESC);
CREATE INDEX IF NOT EXISTS idx_ledger_status ON vulnerability_ledger(challenge_id, status);
