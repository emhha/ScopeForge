-- 0011_v2_backfill_coverage: 从已确认的结构化 vuln 事实回填覆盖矩阵。
--
-- 背景: Scout 契约中 attack_surface 曾可能以单对象输出,旧解析器严格要求数组,
-- 整个契约 unparseable → ReportAttackSurface 从未执行 → coverage_matrix 为空。
-- 漏洞确认事实(facts: prefix=vuln:,state=confirmed,cwe/asset 齐全)是确定性
-- 数据源,可无损补建对应 confirmed 格子;重复键由 idx_coverage_key 幂等拦截。

INSERT OR IGNORE INTO coverage_matrix
  (challenge_id, cwe, asset, endpoint, status, form, skip_reason, created_at, updated_at)
SELECT
  f.challenge_id,
  NULLIF(TRIM(COALESCE(f.cwe, '')), ''),
  TRIM(f.asset),
  NULLIF(TRIM(COALESCE(f.endpoint, '')), ''),
  'confirmed',
  CASE
    WHEN LOWER(COALESCE(f.endpoint, '')) LIKE '%upload%' THEN 'upload'
    WHEN LOWER(COALESCE(f.endpoint, '')) LIKE '%download%'
      OR LOWER(COALESCE(f.endpoint, '')) LIKE '%file%'
      OR LOWER(COALESCE(f.endpoint, '')) LIKE '%static%' THEN 'file'
    WHEN LOWER(COALESCE(f.endpoint, '')) LIKE '%login%'
      OR LOWER(COALESCE(f.endpoint, '')) LIKE '%auth%'
      OR LOWER(COALESCE(f.endpoint, '')) LIKE '%signin%'
      OR LOWER(COALESCE(f.endpoint, '')) LIKE '%password%' THEN 'auth'
    ELSE 'unknown'
  END,
  NULL,
  COALESCE(f.created_at, CAST(strftime('%s', 'now') AS INTEGER)),
  COALESCE(f.created_at, CAST(strftime('%s', 'now') AS INTEGER))
FROM facts f
WHERE f.prefix = 'vuln:'
  AND f.state = 'confirmed'
  AND TRIM(COALESCE(f.cwe, '')) <> ''
  AND TRIM(COALESCE(f.asset, '')) <> ''
GROUP BY
  f.challenge_id,
  COALESCE(f.cwe, ''),
  TRIM(f.asset),
  COALESCE(f.endpoint, '');
