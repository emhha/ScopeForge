-- 0006_v2: 结构化发现字段——幂等键双轨与覆盖矩阵的地基
-- 设计契约见 docs/phase2/03 §1。facts 增可选结构化键(cwe/asset/endpoint/severity),
-- intents 增 target/approach(多样性过滤与接力判定依据,03 §1.3)。

-- facts:结构化键(归一化后)命中 → ErrNoChange;结构化键不全时退回逐字(轨 2)
ALTER TABLE facts ADD COLUMN cwe TEXT;
ALTER TABLE facts ADD COLUMN asset TEXT;
ALTER TABLE facts ADD COLUMN endpoint TEXT;
ALTER TABLE facts ADD COLUMN severity TEXT;

CREATE INDEX IF NOT EXISTS idx_facts_structured ON facts(challenge_id, prefix, cwe, asset, endpoint);

-- intents:target + approach 可选结构化键(防抖双轨 + 多样性过滤判定依据)
ALTER TABLE intents ADD COLUMN target TEXT;
ALTER TABLE intents ADD COLUMN approach TEXT;

CREATE INDEX IF NOT EXISTS idx_intents_structured ON intents(challenge_id, target, approach);
