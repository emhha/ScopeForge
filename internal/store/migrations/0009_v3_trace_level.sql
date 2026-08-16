-- 0009_v3_trace_level: events 表增加 trace_id 与 level 字段
-- Phase 4 M4: 结构化日志——分布式追踪 + 日志级别

ALTER TABLE events ADD COLUMN trace_id TEXT DEFAULT '';
ALTER TABLE events ADD COLUMN level TEXT DEFAULT 'info';

CREATE INDEX IF NOT EXISTS idx_events_trace ON events(trace_id) WHERE trace_id != '';
CREATE INDEX IF NOT EXISTS idx_events_level ON events(level);
