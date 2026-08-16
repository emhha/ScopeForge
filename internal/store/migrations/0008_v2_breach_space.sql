-- 0008_v2: breach 可达状态图(状态节点 + 转移边)——docs/phase2/03 §3.4.1
-- goal_shape=breach 时,接力载体从覆盖矩阵格子换成状态转移边。
-- 状态节点 = 已确认状态(主机/权限/凭据);边 = 转移动作(待尝试/成功/死路)。
-- space_closed 终止(02 §2.2 ②'')= 所有开放边均终态。

CREATE TABLE IF NOT EXISTS breach_nodes (
  id           TEXT PRIMARY KEY,          -- 节点 id(如 shell@host-a)
  challenge_id TEXT NOT NULL,
  kind         TEXT NOT NULL,             -- web|shell|domain_user|host|credential
  asset        TEXT NOT NULL,
  privilege    TEXT,                      -- 权限形态(驱动转移候选生成)
  confirmed    INTEGER NOT NULL DEFAULT 0, -- 是否已确认(独立验证器确认)
  created_at   INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_breach_nodes ON breach_nodes(challenge_id);

CREATE TABLE IF NOT EXISTS breach_edges (
  id           TEXT PRIMARY KEY,          -- 边 id(from|action)
  challenge_id TEXT NOT NULL,
  from_node    TEXT NOT NULL,             -- 源状态(须已确认)
  action       TEXT NOT NULL,             -- 转移动作(横向移动/提权/凭据收集/注入RCE/上传RCE/SSRF...)
  to_node      TEXT,                      -- 目标状态(达成后填,可空=未达成)
  status       TEXT NOT NULL DEFAULT 'open', -- open|claimed|confirmed|dead|skipped
  skip_reason  TEXT,
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_breach_edges ON breach_edges(challenge_id, status);
