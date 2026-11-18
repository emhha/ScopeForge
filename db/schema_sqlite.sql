-- ARTEX SQLite schema. JSON_TEXT has TEXT affinity so JSON numbers retain their
-- precision. Indexed task_asset_links serves hot task/asset membership queries.
-- The opener applies this schema atomically and records its version.

CREATE TABLE IF NOT EXISTS "activity" (
    "id" INTEGER PRIMARY KEY AUTOINCREMENT,
    "exploration_id" INTEGER NOT NULL,
    "node_id" INTEGER,
    "worker" TEXT,
    "kind" TEXT,
    "tool" TEXT,
    "tool_use_id" TEXT,
    "is_error" BOOLEAN NOT NULL DEFAULT false,
    "summary" TEXT,
    "detail" TEXT,
    "metadata" JSON_TEXT NOT NULL DEFAULT '{}' CHECK ("metadata" IS NULL OR json_valid("metadata")),
    "input_tokens" INTEGER,
    "output_tokens" INTEGER,
    "cache_read_tokens" INTEGER,
    "cache_write_tokens" INTEGER,
    "created_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    "main_seg" INTEGER,
    FOREIGN KEY (node_id) REFERENCES exploration_nodes(id) ON DELETE SET NULL,
    FOREIGN KEY (exploration_id) REFERENCES explorations(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS "agent_prompt_vars" (
    "id" INTEGER PRIMARY KEY AUTOINCREMENT,
    "agent_id" INTEGER NOT NULL,
    "var_name" TEXT NOT NULL,
    "description" TEXT,
    "example" TEXT,
    "source" TEXT NOT NULL,
    CHECK ((source IN ('exploration', 'runtime', 'distilled'))),
    UNIQUE (agent_id, var_name),
    FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS "agent_prompts" (
    "id" INTEGER PRIMARY KEY AUTOINCREMENT,
    "agent_id" INTEGER NOT NULL,
    "version" INTEGER NOT NULL,
    "template_text" TEXT NOT NULL,
    "note" TEXT,
    "updated_by" TEXT,
    "created_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    UNIQUE (agent_id, version),
    FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS "agent_skill_visibility" (
    "agent_id" INTEGER NOT NULL,
    "skill_name" TEXT NOT NULL,
    "enabled" BOOLEAN NOT NULL DEFAULT true,
    "created_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    PRIMARY KEY (agent_id, skill_name),
    FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS "agent_triggers" (
    "id" INTEGER PRIMARY KEY AUTOINCREMENT,
    "agent_key" TEXT NOT NULL,
    "enabled" BOOLEAN NOT NULL DEFAULT true,
    "interval_sec" INTEGER NOT NULL DEFAULT 0,
    "on_finding" BOOLEAN NOT NULL DEFAULT false,
    "on_goal_met" BOOLEAN NOT NULL DEFAULT false,
    "on_task_timeout" BOOLEAN NOT NULL DEFAULT false,
    "on_tool_call" BOOLEAN NOT NULL DEFAULT false,
    "on_task_create" BOOLEAN NOT NULL DEFAULT false,
    "interval_message" TEXT NOT NULL DEFAULT '',
    "finding_message" TEXT NOT NULL DEFAULT '',
    "goal_message" TEXT NOT NULL DEFAULT '',
    "task_timeout_message" TEXT NOT NULL DEFAULT '',
    "tool_call_message" TEXT NOT NULL DEFAULT '',
    "task_create_message" TEXT NOT NULL DEFAULT '',
    "tool_names" TEXT NOT NULL DEFAULT '',
    "last_fire" TIMESTAMP,
    "created_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    "updated_at" TIMESTAMP NOT NULL DEFAULT (utc_now())
);

CREATE TRIGGER IF NOT EXISTS "trg_agent_triggers_updated" AFTER UPDATE ON "agent_triggers"
WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE "agent_triggers" SET updated_at=utc_now() WHERE "id"=NEW."id"; END;

CREATE TABLE IF NOT EXISTS "agent_visibility" (
    "agent_id" INTEGER NOT NULL,
    "resource_kind" TEXT NOT NULL,
    "resource_id" INTEGER NOT NULL,
    "mcp_tool_name" TEXT NOT NULL DEFAULT '',
    "enabled" BOOLEAN NOT NULL DEFAULT true,
    "created_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    CHECK ((resource_kind = 'mcp')),
    PRIMARY KEY (agent_id, resource_kind, resource_id, mcp_tool_name),
    FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS "agents" (
    "id" INTEGER PRIMARY KEY AUTOINCREMENT,
    "key" TEXT NOT NULL,
    "name" TEXT NOT NULL,
    "description" TEXT,
    "role" TEXT NOT NULL,
    "builtin" BOOLEAN NOT NULL DEFAULT true,
    "enabled" BOOLEAN NOT NULL DEFAULT true,
    "llm_profile_id" INTEGER,
    "current_prompt_id" INTEGER,
    "max_turns" INTEGER NOT NULL DEFAULT 0,
    "run_seconds" INTEGER NOT NULL DEFAULT 1200,
    "web_search" BOOLEAN NOT NULL DEFAULT false,
    "interactive_shell" BOOLEAN NOT NULL DEFAULT false,
    "wrapup_prompt" TEXT NOT NULL DEFAULT '',
    "wrapup_max_turns" INTEGER NOT NULL DEFAULT 0,
    "task_timeout_wrapup_prompt" TEXT NOT NULL DEFAULT '',
    "task_timeout_wrapup_max_turns" INTEGER NOT NULL DEFAULT 0,
    "trigger_run_mode" TEXT NOT NULL DEFAULT 'serial',
    "trigger_merge_mode" TEXT NOT NULL DEFAULT 'all',
    "trigger_max_parallel" INTEGER NOT NULL DEFAULT 5,
    "created_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    "updated_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    CHECK ((key REGEXP '^[a-z][a-z0-9_]*$')),
    FOREIGN KEY (current_prompt_id) REFERENCES agent_prompts(id) ON DELETE SET NULL,
    CHECK ((trigger_run_mode IN ('serial', 'parallel'))),
    CHECK ((trigger_merge_mode IN ('by_task', 'all', 'none'))),
    CHECK ((role IN ('goals', 'main', 'planner', 'worker', 'assistant'))),
    UNIQUE (key),
    FOREIGN KEY (llm_profile_id) REFERENCES llm_profiles(id) ON DELETE SET NULL
);

CREATE TRIGGER IF NOT EXISTS "trg_agents_updated" AFTER UPDATE ON "agents"
WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE "agents" SET updated_at=utc_now() WHERE "id"=NEW."id"; END;

CREATE TABLE IF NOT EXISTS "asset_intercept_rules" (
    "id" INTEGER PRIMARY KEY AUTOINCREMENT,
    "enabled" BOOLEAN NOT NULL DEFAULT true,
    "kind" TEXT NOT NULL,
    "pattern" TEXT NOT NULL,
    "note" TEXT NOT NULL DEFAULT '',
    "builtin" BOOLEAN NOT NULL DEFAULT false,
    "created_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    "updated_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    CHECK ((kind IN ('exact_domain', 'exact_ip', 'exact_url', 'fuzzy_domain', 'fuzzy_ip', 'fuzzy_url', 'cidr')))
);

CREATE TRIGGER IF NOT EXISTS "trg_asset_intercept_rules_updated" AFTER UPDATE ON "asset_intercept_rules"
WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE "asset_intercept_rules" SET updated_at=utc_now() WHERE "id"=NEW."id"; END;

CREATE TABLE IF NOT EXISTS "assets" (
    "id" INTEGER PRIMARY KEY AUTOINCREMENT,
    "type" TEXT NOT NULL,
    "company_id" INTEGER,
    "company_source" TEXT NOT NULL DEFAULT 'explicit',
    "task_ids" JSON_TEXT NOT NULL DEFAULT '[]' CHECK ("task_ids" IS NULL OR json_valid("task_ids")),
    "domain" TEXT,
    "root_domain" TEXT,
    "ip" TEXT,
    "c_segment" TEXT,
    "port" INTEGER,
    "icp" TEXT,
    "bound_domains" JSON_TEXT NOT NULL DEFAULT '[]' CHECK ("bound_domains" IS NULL OR json_valid("bound_domains")),
    "open_ports" JSON_TEXT NOT NULL DEFAULT '[]' CHECK ("open_ports" IS NULL OR json_valid("open_ports")),
    "record_type" TEXT,
    "record_value" JSON_TEXT CHECK ("record_value" IS NULL OR json_valid("record_value")),
    "bundle_id" TEXT,
    "app_name" TEXT,
    "category" TEXT,
    "app_description" TEXT,
    "app_icp" TEXT,
    "url" TEXT,
    "service_type" TEXT,
    "service_name" TEXT,
    "favicon_mmh3" TEXT,
    "status_code" INTEGER,
    "content_length" INTEGER,
    "page_title" TEXT,
    "technologies" JSON_TEXT NOT NULL DEFAULT '[]' CHECK ("technologies" IS NULL OR json_valid("technologies")),
    "auth" JSON_TEXT NOT NULL DEFAULT '[]' CHECK ("auth" IS NULL OR json_valid("auth")),
    "method" TEXT,
    "params" JSON_TEXT NOT NULL DEFAULT '[]' CHECK ("params" IS NULL OR json_valid("params")),
    "extra" JSON_TEXT NOT NULL DEFAULT '{}' CHECK ("extra" IS NULL OR json_valid("extra")),
    "created_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    "last_seen" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    "updated_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    CHECK ((type IN ('root_domain', 'ip', 'subdomain', 'app', 'service', 'endpoint'))),
    CHECK (((port >= 1) AND (port <= 65535))),
    CHECK ((service_type IN ('http', 'other'))),
    FOREIGN KEY (company_id) REFERENCES companies(id) ON DELETE SET NULL,
    CHECK ((company_source IN ('explicit', 'scope')))
);

CREATE TRIGGER IF NOT EXISTS "trg_assets_updated" AFTER UPDATE ON "assets"
WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE "assets" SET updated_at=utc_now() WHERE "id"=NEW."id"; END;

CREATE TABLE IF NOT EXISTS "companies" (
    "id" INTEGER PRIMARY KEY AUTOINCREMENT,
    "name" TEXT NOT NULL,
    "nkey" TEXT NOT NULL,
    "logo" TEXT,
    "created_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    "updated_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    UNIQUE (nkey)
);

CREATE TRIGGER IF NOT EXISTS "trg_companies_updated" AFTER UPDATE ON "companies"
WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE "companies" SET updated_at=utc_now() WHERE "id"=NEW."id"; END;

CREATE TABLE IF NOT EXISTS "company_scope" (
    "id" INTEGER PRIMARY KEY AUTOINCREMENT,
    "company_id" INTEGER NOT NULL,
    "kind" TEXT NOT NULL,
    "domain" TEXT,
    "net" TEXT,
    "value" TEXT,
    "raw" TEXT NOT NULL,
    "reason" TEXT,
    "created_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    UNIQUE (company_id, domain),
    UNIQUE (company_id, net),
    FOREIGN KEY (company_id) REFERENCES companies(id) ON DELETE CASCADE,
    CHECK ((kind IN ('domain', 'ip', 'cidr', 'icp', 'keyword'))),
    CHECK ((((kind = 'domain') AND (domain IS NOT NULL) AND (net IS NULL) AND (value IS NULL)) OR ((kind IN ('ip', 'cidr')) AND (domain IS NULL) AND (net IS NOT NULL) AND (value IS NULL)) OR ((kind IN ('icp', 'keyword')) AND (domain IS NULL) AND (net IS NULL) AND (value IS NOT NULL))))
);

CREATE TABLE IF NOT EXISTS "conversation_activities" (
    "id" INTEGER PRIMARY KEY AUTOINCREMENT,
    "conversation_id" INTEGER NOT NULL,
    "worker" TEXT,
    "kind" TEXT,
    "tool" TEXT,
    "tool_use_id" TEXT,
    "is_error" BOOLEAN NOT NULL DEFAULT false,
    "summary" TEXT,
    "detail" TEXT,
    "input_tokens" INTEGER,
    "output_tokens" INTEGER,
    "cache_read_tokens" INTEGER,
    "cache_write_tokens" INTEGER,
    "created_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    FOREIGN KEY (conversation_id) REFERENCES conversations(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS "conversations" (
    "id" INTEGER PRIMARY KEY AUTOINCREMENT,
    "agent_key" TEXT NOT NULL,
    "title" TEXT NOT NULL DEFAULT '',
    "llm_profile_id" INTEGER,
    "pinned_at" TIMESTAMP,
    "created_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    "updated_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    FOREIGN KEY (llm_profile_id) REFERENCES llm_profiles(id) ON DELETE SET NULL
);

CREATE TRIGGER IF NOT EXISTS "trg_conversations_updated" AFTER UPDATE ON "conversations"
WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE "conversations" SET updated_at=utc_now() WHERE "id"=NEW."id"; END;

CREATE TABLE IF NOT EXISTS "exploration_anchors" (
    "node_id" INTEGER NOT NULL,
    "asset_id" INTEGER NOT NULL,
    PRIMARY KEY (node_id, asset_id),
    FOREIGN KEY (node_id) REFERENCES exploration_nodes(id) ON DELETE CASCADE,
    FOREIGN KEY (asset_id) REFERENCES assets(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS "exploration_edges" (
    "exploration_id" INTEGER NOT NULL,
    "src_id" INTEGER NOT NULL,
    "dst_id" INTEGER NOT NULL,
    "rel" TEXT NOT NULL,
    "created_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    CHECK ((src_id <> dst_id)),
    CHECK ((rel IN ('spawns', 'derived_from', 'yields', 'proves', 'covers'))),
    PRIMARY KEY (exploration_id, src_id, rel, dst_id),
    FOREIGN KEY (exploration_id) REFERENCES explorations(id) ON DELETE CASCADE,
    FOREIGN KEY (src_id) REFERENCES exploration_nodes(id) ON DELETE CASCADE,
    FOREIGN KEY (dst_id) REFERENCES exploration_nodes(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS "exploration_nodes" (
    "id" INTEGER PRIMARY KEY AUTOINCREMENT,
    "exploration_id" INTEGER NOT NULL,
    "kind" TEXT NOT NULL,
    "payload" JSON_TEXT NOT NULL DEFAULT '{}' CHECK ("payload" IS NULL OR json_valid("payload")),
    "priority" INTEGER NOT NULL DEFAULT 0,
    "state" TEXT NOT NULL DEFAULT 'open',
    "origin" TEXT,
    "owner" TEXT,
    "blocked_reason" TEXT,
    "delete_reason" TEXT,
    "created_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    "updated_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    "completed_at" TIMESTAMP,
    "content_version" INTEGER NOT NULL DEFAULT 0,
    "cold_since_round" INTEGER,
    CHECK ((kind IN ('begin', 'goal', 'intent', 'fact', 'finding', 'hint', 'digest'))),
    CHECK ((((kind = 'begin') AND (state = 'open')) OR ((kind = 'intent') AND (state IN ('open', 'running', 'paused', 'done', 'blocked', 'exhausted', 'stopped', 'deleted'))) OR ((kind = 'goal') AND (state IN ('open', 'met', 'abandoned'))) OR ((kind = 'fact') AND (state IN ('confirmed', 'dismissed', 'origin'))) OR ((kind = 'finding') AND (state IN ('confirmed', 'dismissed'))) OR ((kind = 'hint') AND (state IN ('active', 'consumed'))) OR ((kind = 'digest') AND (state IN ('active', 'superseded'))))),
    FOREIGN KEY (exploration_id) REFERENCES explorations(id) ON DELETE CASCADE
);

CREATE TRIGGER IF NOT EXISTS "trg_exploration_nodes_updated" AFTER UPDATE ON "exploration_nodes"
WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE "exploration_nodes" SET updated_at=utc_now() WHERE "id"=NEW."id"; END;

CREATE TABLE IF NOT EXISTS "explorations" (
    "id" INTEGER PRIMARY KEY AUTOINCREMENT,
    "description" TEXT,
    "goal" TEXT NOT NULL,
    "status" TEXT NOT NULL DEFAULT 'open',
    "created_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    "updated_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    "round_no" INTEGER NOT NULL DEFAULT 0,
    CHECK ((status IN ('open', 'achieved', 'failed')))
);

CREATE TRIGGER IF NOT EXISTS "trg_explorations_updated" AFTER UPDATE ON "explorations"
WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE "explorations" SET updated_at=utc_now() WHERE "id"=NEW."id"; END;

CREATE TABLE IF NOT EXISTS "finding_retests" (
    "id" INTEGER PRIMARY KEY AUTOINCREMENT,
    "finding_id" INTEGER NOT NULL,
    "conversation_id" INTEGER,
    "status" TEXT NOT NULL DEFAULT 'pending',
    "verdict" TEXT NOT NULL DEFAULT '',
    "notes" TEXT NOT NULL DEFAULT '',
    "snapshot" JSON_TEXT NOT NULL CHECK ("snapshot" IS NULL OR json_valid("snapshot")),
    "summary" TEXT NOT NULL DEFAULT '',
    "evidence" TEXT NOT NULL DEFAULT '',
    "error" TEXT NOT NULL DEFAULT '',
    "created_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    "started_at" TIMESTAMP,
    "finished_at" TIMESTAMP,
    CHECK ((status IN ('pending', 'running', 'completed', 'failed', 'stopped'))),
    CHECK ((verdict IN ('', 'reproduced', 'fixed', 'inconclusive'))),
    UNIQUE (conversation_id),
    FOREIGN KEY (finding_id) REFERENCES findings(id) ON DELETE CASCADE,
    FOREIGN KEY (conversation_id) REFERENCES conversations(id) ON DELETE SET NULL
);

CREATE TABLE IF NOT EXISTS "finding_traffic_bindings" (
    "id" INTEGER PRIMARY KEY AUTOINCREMENT,
    "finding_id" INTEGER NOT NULL,
    "snapshot_id" TEXT NOT NULL,
    "role" TEXT NOT NULL DEFAULT 'supporting',
    "note" TEXT NOT NULL DEFAULT '',
    "position" INTEGER NOT NULL,
    "created_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    UNIQUE (finding_id, snapshot_id),
    FOREIGN KEY (finding_id) REFERENCES findings(id) ON DELETE CASCADE,
    FOREIGN KEY (snapshot_id) REFERENCES traffic_evidence_snapshots(id)
);

CREATE TABLE IF NOT EXISTS "findings" (
    "id" INTEGER PRIMARY KEY AUTOINCREMENT,
    "task_id" INTEGER,
    "node_id" INTEGER,
    "vulnclass" TEXT NOT NULL DEFAULT '',
    "name" TEXT NOT NULL DEFAULT '',
    "severity" TEXT NOT NULL DEFAULT '',
    "summary" TEXT NOT NULL DEFAULT '',
    "evidence" TEXT NOT NULL DEFAULT '',
    "worker" TEXT NOT NULL DEFAULT '',
    "asset_ids" JSON_TEXT NOT NULL DEFAULT '[]' CHECK ("asset_ids" IS NULL OR json_valid("asset_ids")),
    "status" TEXT NOT NULL DEFAULT 'pending',
    "report" TEXT NOT NULL DEFAULT '',
    "created_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    "evidence_version" INTEGER NOT NULL DEFAULT 0,
    "report_evidence_version" INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE SET NULL,
    FOREIGN KEY (node_id) REFERENCES exploration_nodes(id) ON DELETE SET NULL
);

CREATE TABLE IF NOT EXISTS "intercept_pending" (
    "id" INTEGER PRIMARY KEY AUTOINCREMENT,
    "rule_id" INTEGER,
    "conversation_id" INTEGER,
    "task_id" TEXT,
    "agent_name" TEXT NOT NULL DEFAULT '',
    "tool_name" TEXT NOT NULL,
    "tool_input" JSON_TEXT NOT NULL DEFAULT '{}' CHECK ("tool_input" IS NULL OR json_valid("tool_input")),
    "status" TEXT NOT NULL DEFAULT 'pending',
    "reason" TEXT NOT NULL DEFAULT '',
    "decided_at" TIMESTAMP,
    "created_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    "audit" JSON_TEXT CHECK ("audit" IS NULL OR json_valid("audit")),
    "decision_source" TEXT NOT NULL DEFAULT '',
    CHECK ((status IN ('pending', 'allowed', 'denied', 'timeout'))),
    FOREIGN KEY (rule_id) REFERENCES intercept_rules(id) ON DELETE SET NULL,
    FOREIGN KEY (conversation_id) REFERENCES conversations(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS "intercept_rules" (
    "id" INTEGER PRIMARY KEY AUTOINCREMENT,
    "name" TEXT NOT NULL,
    "enabled" BOOLEAN NOT NULL DEFAULT true,
    "priority" INTEGER NOT NULL DEFAULT 0,
    "match_target" TEXT NOT NULL,
    "match_type" TEXT NOT NULL,
    "pattern" TEXT NOT NULL,
    "action" TEXT NOT NULL,
    "message" TEXT NOT NULL DEFAULT '',
    "timeout_enabled" BOOLEAN NOT NULL DEFAULT true,
    "timeout_seconds" INTEGER NOT NULL DEFAULT 60,
    "timeout_action" TEXT NOT NULL DEFAULT 'deny',
    "created_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    "updated_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    CHECK ((match_target IN ('tool_name', 'tool_input'))),
    CHECK ((match_type IN ('string', 'regex'))),
    CHECK ((action IN ('allow', 'deny', 'ask')))
);

CREATE TRIGGER IF NOT EXISTS "trg_intercept_rules_updated" AFTER UPDATE ON "intercept_rules"
WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE "intercept_rules" SET updated_at=utc_now() WHERE "id"=NEW."id"; END;

CREATE TABLE IF NOT EXISTS "llm_profile_health" (
    "profile_id" INTEGER PRIMARY KEY AUTOINCREMENT,
    "fails" INTEGER NOT NULL DEFAULT 0,
    "trips" INTEGER NOT NULL DEFAULT 0,
    "open_until" TIMESTAMP,
    "last_error" TEXT NOT NULL DEFAULT '',
    "last_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    FOREIGN KEY (profile_id) REFERENCES llm_profiles(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS "llm_profiles" (
    "id" INTEGER PRIMARY KEY AUTOINCREMENT,
    "name" TEXT NOT NULL,
    "format" TEXT NOT NULL,
    "base_url" TEXT,
    "proxy" TEXT,
    "model" TEXT NOT NULL,
    "api_key" TEXT,
    "api_key_hint" TEXT,
    "rate_per_second" REAL NOT NULL DEFAULT 0,
    "rate_per_minute" REAL NOT NULL DEFAULT 0,
    "context_window_k" INTEGER NOT NULL DEFAULT 0,
    "reasoning_effort" TEXT NOT NULL DEFAULT '',
    "thinking_type" TEXT NOT NULL DEFAULT '',
    "is_default" BOOLEAN NOT NULL DEFAULT false,
    "priority" INTEGER NOT NULL DEFAULT 0,
    "pool_exclude" BOOLEAN NOT NULL DEFAULT false,
    "streaming" BOOLEAN NOT NULL DEFAULT true,
    "max_tokens" INTEGER NOT NULL DEFAULT 0,
    "max_tokens_field" TEXT NOT NULL DEFAULT '',
    "session_header_key" TEXT NOT NULL DEFAULT '',
    "retry_connect_attempts" INTEGER NOT NULL DEFAULT 0,
    "retry_connect_interval_ms" INTEGER NOT NULL DEFAULT 0,
    "retry_empty_attempts" INTEGER NOT NULL DEFAULT 0,
    "retry_empty_interval_ms" INTEGER NOT NULL DEFAULT 0,
    "retry_stream_attempts" INTEGER NOT NULL DEFAULT 0,
    "retry_stream_interval_ms" INTEGER NOT NULL DEFAULT 0,
    "created_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    "updated_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    UNIQUE (name),
    CHECK ((format IN ('openai', 'anthropic', 'openai-responses'))),
    CHECK ((max_tokens_field IN ('', 'max_completion_tokens'))),
    CHECK ((max_tokens >= 0)),
    CHECK (((retry_connect_attempts >= '-1') AND (retry_empty_attempts >= '-1') AND (retry_stream_attempts >= '-1') AND (retry_connect_interval_ms >= 0) AND (retry_empty_interval_ms >= 0) AND (retry_stream_interval_ms >= 0)))
);

CREATE TRIGGER IF NOT EXISTS "trg_llm_profiles_updated" AFTER UPDATE ON "llm_profiles"
WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE "llm_profiles" SET updated_at=utc_now() WHERE "id"=NEW."id"; END;

CREATE TABLE IF NOT EXISTS "llm_records" (
    "id" INTEGER PRIMARY KEY AUTOINCREMENT,
    "ts" TIMESTAMP DEFAULT (utc_now()),
    "model" TEXT,
    "profile_name" TEXT,
    "session_id" TEXT,
    "task_id" TEXT,
    "worker" TEXT,
    "latency_ms" INTEGER,
    "input_tokens" INTEGER,
    "output_tokens" INTEGER,
    "cache_read" INTEGER,
    "cache_write" INTEGER,
    "status" TEXT,
    "error" TEXT,
    "request_body" TEXT,
    "response_body" TEXT,
    "raw_request" TEXT,
    "raw_response" TEXT
);

CREATE TABLE IF NOT EXISTS "llm_usage" (
    "id" INTEGER PRIMARY KEY AUTOINCREMENT,
    "ts" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    "task_id" TEXT,
    "exploration_id" INTEGER,
    "worker" TEXT,
    "model" TEXT,
    "profile_name" TEXT,
    "latency_ms" INTEGER,
    "input_tokens" INTEGER NOT NULL DEFAULT 0,
    "output_tokens" INTEGER NOT NULL DEFAULT 0,
    "cache_read" INTEGER NOT NULL DEFAULT 0,
    "cache_write" INTEGER NOT NULL DEFAULT 0,
    "status" TEXT
);

CREATE TABLE IF NOT EXISTS "main_sessions" (
    "exploration_id" INTEGER NOT NULL,
    "seq" INTEGER NOT NULL,
    "created_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    PRIMARY KEY (exploration_id, seq),
    FOREIGN KEY (exploration_id) REFERENCES explorations(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS "mcp_servers" (
    "id" INTEGER PRIMARY KEY AUTOINCREMENT,
    "name" TEXT NOT NULL,
    "transport" TEXT NOT NULL,
    "command" TEXT,
    "args" JSON_TEXT NOT NULL DEFAULT '[]' CHECK ("args" IS NULL OR json_valid("args")),
    "env" JSON_TEXT NOT NULL DEFAULT '{}' CHECK ("env" IS NULL OR json_valid("env")),
    "url" TEXT,
    "enabled" BOOLEAN NOT NULL DEFAULT true,
    "insecure" BOOLEAN NOT NULL DEFAULT false,
    "created_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    "updated_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    UNIQUE (name),
    CHECK ((transport IN ('stdio', 'http', 'sse')))
);

CREATE TRIGGER IF NOT EXISTS "trg_mcp_servers_updated" AFTER UPDATE ON "mcp_servers"
WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE "mcp_servers" SET updated_at=utc_now() WHERE "id"=NEW."id"; END;

CREATE TABLE IF NOT EXISTS "mcp_tools_cache" (
    "id" INTEGER PRIMARY KEY AUTOINCREMENT,
    "server_id" INTEGER NOT NULL,
    "tool_name" TEXT NOT NULL,
    "description" TEXT,
    "schema" JSON_TEXT CHECK ("schema" IS NULL OR json_valid("schema")),
    "discovered_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    UNIQUE (server_id, tool_name),
    FOREIGN KEY (server_id) REFERENCES mcp_servers(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS "notification_channels" (
    "id" INTEGER PRIMARY KEY AUTOINCREMENT,
    "name" TEXT NOT NULL,
    "kind" TEXT NOT NULL,
    "enabled" BOOLEAN NOT NULL DEFAULT true,
    "config" JSON_TEXT NOT NULL DEFAULT '{}' CHECK ("config" IS NULL OR json_valid("config")),
    "mode" TEXT NOT NULL DEFAULT 'realtime',
    "filter" JSON_TEXT NOT NULL DEFAULT '{}' CHECK ("filter" IS NULL OR json_valid("filter")),
    "rate_per_min" INTEGER NOT NULL DEFAULT 20,
    "created_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    "updated_at" TIMESTAMP NOT NULL DEFAULT (utc_now())
);

CREATE TRIGGER IF NOT EXISTS "trg_notification_channels_updated" AFTER UPDATE ON "notification_channels"
WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE "notification_channels" SET updated_at=utc_now() WHERE "id"=NEW."id"; END;

CREATE TABLE IF NOT EXISTS "notification_deliveries" (
    "id" INTEGER PRIMARY KEY AUTOINCREMENT,
    "event_id" INTEGER NOT NULL,
    "channel_id" INTEGER NOT NULL,
    "state" TEXT NOT NULL DEFAULT 'pending',
    "attempts" INTEGER NOT NULL DEFAULT 0,
    "next_attempt_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    "last_error" TEXT NOT NULL DEFAULT '',
    "batch_id" INTEGER,
    "created_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    "sent_at" TIMESTAMP,
    FOREIGN KEY (event_id) REFERENCES notification_events(id) ON DELETE CASCADE,
    FOREIGN KEY (channel_id) REFERENCES notification_channels(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS "notification_events" (
    "id" INTEGER PRIMARY KEY AUTOINCREMENT,
    "kind" TEXT NOT NULL,
    "finding_id" INTEGER NOT NULL,
    "snapshot" JSON_TEXT NOT NULL CHECK ("snapshot" IS NULL OR json_valid("snapshot")),
    "fanned_out" BOOLEAN NOT NULL DEFAULT false,
    "created_at" TIMESTAMP NOT NULL DEFAULT (utc_now())
);

CREATE TABLE IF NOT EXISTS "scheduler_state" (
    "key" TEXT NOT NULL,
    "value" TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (key)
);

CREATE TABLE IF NOT EXISTS "server_logs" (
    "id" INTEGER PRIMARY KEY AUTOINCREMENT,
    "created_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    "level" TEXT NOT NULL DEFAULT 'info',
    "tag" TEXT NOT NULL DEFAULT '',
    "text" TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS "settings" (
    "key" TEXT NOT NULL,
    "value" TEXT NOT NULL,
    "updated_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    PRIMARY KEY (key)
);

CREATE TRIGGER IF NOT EXISTS "trg_settings_updated" AFTER UPDATE ON "settings"
WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE "settings" SET updated_at=utc_now() WHERE "key"=NEW."key"; END;

CREATE TABLE IF NOT EXISTS "side_question_requests" (
    "id" TEXT NOT NULL,
    "ordinal" INTEGER PRIMARY KEY AUTOINCREMENT,
    "session_key" TEXT NOT NULL,
    "generation" INTEGER NOT NULL,
    "client_id" TEXT NOT NULL,
    "question" TEXT NOT NULL,
    "answer" TEXT NOT NULL DEFAULT '',
    "status" TEXT NOT NULL,
    "error" TEXT NOT NULL DEFAULT '',
    "model" JSON_TEXT NOT NULL CHECK ("model" IS NULL OR json_valid("model")),
    "snapshot_at" TIMESTAMP NOT NULL,
    "created_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    "sequence" INTEGER NOT NULL DEFAULT 0,
    "usage" JSON_TEXT NOT NULL DEFAULT '{}' CHECK ("usage" IS NULL OR json_valid("usage")),
    "context_info" JSON_TEXT NOT NULL DEFAULT '{}' CHECK ("context_info" IS NULL OR json_valid("context_info")),
    CHECK ((status IN ('running', 'completed', 'failed', 'cancelled', 'interrupted'))),
    UNIQUE (id),
    UNIQUE (session_key, generation, client_id),
    FOREIGN KEY (session_key) REFERENCES side_question_sessions(session_key) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS "side_question_sessions" (
    "session_key" TEXT NOT NULL,
    "conversation_id" INTEGER,
    "task_id" INTEGER,
    "exploration_id" INTEGER,
    "intent_id" INTEGER,
    "run_id" INTEGER NOT NULL,
    "version" INTEGER NOT NULL,
    "snapshot" JSON_TEXT NOT NULL CHECK ("snapshot" IS NULL OR json_valid("snapshot")),
    "generation" INTEGER NOT NULL DEFAULT 0,
    "memory" JSON_TEXT NOT NULL DEFAULT '{}' CHECK ("memory" IS NULL OR json_valid("memory")),
    CHECK ((((conversation_id IS NOT NULL) AND (task_id IS NULL) AND (exploration_id IS NULL) AND (intent_id IS NULL)) OR ((conversation_id IS NULL) AND (task_id IS NOT NULL) AND (exploration_id IS NOT NULL)))),
    PRIMARY KEY (session_key),
    FOREIGN KEY (conversation_id) REFERENCES conversations(id) ON DELETE CASCADE,
    FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE CASCADE,
    FOREIGN KEY (exploration_id) REFERENCES explorations(id) ON DELETE CASCADE,
    FOREIGN KEY (intent_id) REFERENCES exploration_nodes(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS "skill_usage" (
    "id" INTEGER PRIMARY KEY AUTOINCREMENT,
    "ts" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    "skill" TEXT NOT NULL,
    "agent_key" TEXT,
    "task_id" INTEGER,
    "exploration_id" INTEGER,
    "intent_id" INTEGER,
    "session_id" TEXT,
    "args_len" INTEGER NOT NULL DEFAULT 0,
    "found" BOOLEAN NOT NULL DEFAULT true
);

CREATE TABLE IF NOT EXISTS "task_archives" (
    "id" INTEGER PRIMARY KEY AUTOINCREMENT,
    "task_id" INTEGER NOT NULL,
    "state" TEXT NOT NULL DEFAULT 'archive_queued',
    "phase" TEXT NOT NULL DEFAULT 'queued',
    "progress" INTEGER NOT NULL DEFAULT 0,
    "error" TEXT NOT NULL DEFAULT '',
    "warnings" JSON_TEXT NOT NULL DEFAULT '[]' CHECK ("warnings" IS NULL OR json_valid("warnings")),
    "format_version" INTEGER NOT NULL DEFAULT 2,
    "archive_path" TEXT NOT NULL DEFAULT '',
    "sha256" TEXT NOT NULL DEFAULT '',
    "original_size" INTEGER NOT NULL DEFAULT 0,
    "compressed_size" INTEGER NOT NULL DEFAULT 0,
    "task_name" TEXT NOT NULL DEFAULT '',
    "task_description" TEXT NOT NULL DEFAULT '',
    "task_goal" TEXT NOT NULL DEFAULT '',
    "original_status" TEXT NOT NULL DEFAULT '',
    "category_id_snapshot" INTEGER,
    "category_name_snapshot" TEXT NOT NULL DEFAULT '',
    "source_task_ids" JSON_TEXT NOT NULL DEFAULT '[]' CHECK ("source_task_ids" IS NULL OR json_valid("source_task_ids")),
    "remaining_timeout_seconds" INTEGER NOT NULL DEFAULT 0,
    "data_counts" JSON_TEXT NOT NULL DEFAULT '{}' CHECK ("data_counts" IS NULL OR json_valid("data_counts")),
    "aggregate_stats" JSON_TEXT NOT NULL DEFAULT '{}' CHECK ("aggregate_stats" IS NULL OR json_valid("aggregate_stats")),
    "archived_at" TIMESTAMP,
    "requested_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    "created_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    "updated_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    CHECK ((state IN ('archive_queued', 'archiving', 'archive_failed', 'ready', 'restore_queued', 'restoring', 'restore_failed', 'delete_queued', 'deleting', 'delete_failed'))),
    CHECK (((progress >= 0) AND (progress <= 100))),
    UNIQUE (task_id),
    FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE CASCADE
);

CREATE TRIGGER IF NOT EXISTS "trg_task_archives_updated" AFTER UPDATE ON "task_archives"
WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE "task_archives" SET updated_at=utc_now() WHERE "id"=NEW."id"; END;

CREATE TABLE IF NOT EXISTS "task_asset_links" (
    "task_id" INTEGER NOT NULL,
    "asset_id" INTEGER NOT NULL,
    "source" TEXT NOT NULL DEFAULT 'system',
    "source_summary" TEXT NOT NULL DEFAULT '',
    "source_node_id" INTEGER,
    "created_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    "updated_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    PRIMARY KEY (task_id, asset_id),
    FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE CASCADE,
    FOREIGN KEY (asset_id) REFERENCES assets(id) ON DELETE CASCADE,
    FOREIGN KEY (source_node_id) REFERENCES exploration_nodes(id) ON DELETE SET NULL
);

CREATE TABLE IF NOT EXISTS "task_categories" (
    "id" INTEGER PRIMARY KEY AUTOINCREMENT,
    "name" TEXT NOT NULL,
    "nkey" TEXT NOT NULL,
    "created_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    "updated_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    UNIQUE (nkey)
);

CREATE TRIGGER IF NOT EXISTS "trg_task_categories_updated" AFTER UPDATE ON "task_categories"
WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE "task_categories" SET updated_at=utc_now() WHERE "id"=NEW."id"; END;

CREATE TABLE IF NOT EXISTS "task_constraints" (
    "id" INTEGER PRIMARY KEY AUTOINCREMENT,
    "exploration_id" INTEGER NOT NULL,
    "kind" TEXT NOT NULL,
    "text" TEXT NOT NULL,
    "origin" TEXT,
    "created_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    "updated_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    CHECK ((kind IN ('allow', 'deny'))),
    FOREIGN KEY (exploration_id) REFERENCES explorations(id) ON DELETE CASCADE
);

CREATE TRIGGER IF NOT EXISTS "trg_task_constraints_updated" AFTER UPDATE ON "task_constraints"
WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE "task_constraints" SET updated_at=utc_now() WHERE "id"=NEW."id"; END;

CREATE TABLE IF NOT EXISTS "task_intercept_rules" (
    "id" INTEGER PRIMARY KEY AUTOINCREMENT,
    "task_id" INTEGER NOT NULL,
    "enabled" BOOLEAN NOT NULL DEFAULT true,
    "action" TEXT NOT NULL DEFAULT 'block',
    "kind" TEXT NOT NULL,
    "pattern" TEXT NOT NULL,
    "note" TEXT NOT NULL DEFAULT '',
    "created_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    "updated_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    CHECK ((action IN ('block', 'allow'))),
    CHECK ((kind IN ('exact_domain', 'exact_ip', 'exact_url', 'fuzzy_domain', 'fuzzy_ip', 'fuzzy_url', 'cidr'))),
    FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE CASCADE
);

CREATE TRIGGER IF NOT EXISTS "trg_task_intercept_rules_updated" AFTER UPDATE ON "task_intercept_rules"
WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE "task_intercept_rules" SET updated_at=utc_now() WHERE "id"=NEW."id"; END;

CREATE TABLE IF NOT EXISTS "task_llm_profiles" (
    "task_id" INTEGER NOT NULL,
    "profile_id" INTEGER NOT NULL,
    "position" INTEGER NOT NULL,
    "status" TEXT NOT NULL DEFAULT 'ready',
    "last_error" TEXT,
    "exhausted_at" TIMESTAMP,
    "created_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    "updated_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    CHECK (("position" >= 0)),
    CHECK ((status IN ('ready', 'quota_exhausted'))),
    PRIMARY KEY (task_id, profile_id),
    UNIQUE (task_id, "position"),
    FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE CASCADE,
    FOREIGN KEY (profile_id) REFERENCES llm_profiles(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS "task_relations" (
    "task_id" INTEGER NOT NULL,
    "source_task_id" INTEGER NOT NULL,
    "created_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    CHECK ((task_id <> source_task_id)),
    PRIMARY KEY (task_id, source_task_id),
    FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE CASCADE,
    FOREIGN KEY (source_task_id) REFERENCES tasks(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS "task_scope" (
    "id" INTEGER PRIMARY KEY AUTOINCREMENT,
    "task_id" INTEGER NOT NULL,
    "kind" TEXT NOT NULL,
    "company_id" INTEGER,
    "domain" TEXT,
    "net" TEXT,
    "value" TEXT,
    "source" TEXT NOT NULL DEFAULT 'auto',
    "reason" TEXT,
    "created_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    CHECK ((source IN ('auto', 'agent', 'manual'))),
    FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE CASCADE,
    FOREIGN KEY (company_id) REFERENCES companies(id) ON DELETE CASCADE,
    CHECK ((kind IN ('company', 'root_domain', 'subdomain', 'ip', 'cidr', 'icp', 'keyword')))
);

CREATE TABLE IF NOT EXISTS "task_templates" (
    "id" INTEGER PRIMARY KEY AUTOINCREMENT,
    "name" TEXT NOT NULL,
    "nkey" TEXT NOT NULL,
    "description" TEXT NOT NULL,
    "goal" TEXT NOT NULL,
    "category_id" INTEGER,
    "intercept_rules" JSON_TEXT NOT NULL DEFAULT '[]' CHECK ("intercept_rules" IS NULL OR json_valid("intercept_rules")),
    "created_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    "updated_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    UNIQUE (nkey),
    FOREIGN KEY (category_id) REFERENCES task_categories(id) ON DELETE SET NULL
);

CREATE TRIGGER IF NOT EXISTS "trg_task_templates_updated" AFTER UPDATE ON "task_templates"
WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE "task_templates" SET updated_at=utc_now() WHERE "id"=NEW."id"; END;

CREATE TABLE IF NOT EXISTS "tasks" (
    "id" INTEGER PRIMARY KEY AUTOINCREMENT,
    "name" TEXT NOT NULL DEFAULT '',
    "category_id" INTEGER,
    "description" TEXT NOT NULL,
    "goal" TEXT NOT NULL,
    "exploration_id" INTEGER NOT NULL,
    "status" TEXT NOT NULL DEFAULT 'created',
    "paused" BOOLEAN NOT NULL DEFAULT false,
    "queued" BOOLEAN NOT NULL DEFAULT false,
    "queued_at" TIMESTAMP,
    "queue_mode" TEXT NOT NULL DEFAULT '',
    "llm_profile_id" INTEGER,
    "active_llm_profile_id" INTEGER,
    "llm_chain_revision" INTEGER NOT NULL DEFAULT 0,
    "company_id" INTEGER,
    "parent_ref" TEXT,
    "timeout_seconds" INTEGER NOT NULL DEFAULT 0,
    "plan_heartbeat_seconds" INTEGER NOT NULL DEFAULT 300,
    "coverage_enabled" BOOLEAN NOT NULL DEFAULT true,
    "pinned_at" TIMESTAMP,
    "first_run_at" TIMESTAMP,
    "deadline_at" TIMESTAMP,
    "archived_at" TIMESTAMP,
    "deleted_at" TIMESTAMP,
    "completed_at" TIMESTAMP,
    "created_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    "updated_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    CHECK ((status IN ('created', 'running', 'paused', 'done', 'failed', 'timeout'))),
    UNIQUE (exploration_id),
    FOREIGN KEY (category_id) REFERENCES task_categories(id) ON DELETE SET NULL,
    FOREIGN KEY (exploration_id) REFERENCES explorations(id) ON DELETE RESTRICT,
    FOREIGN KEY (llm_profile_id) REFERENCES llm_profiles(id) ON DELETE SET NULL,
    FOREIGN KEY (active_llm_profile_id) REFERENCES llm_profiles(id) ON DELETE SET NULL,
    FOREIGN KEY (company_id) REFERENCES companies(id) ON DELETE SET NULL
);

CREATE TRIGGER IF NOT EXISTS "trg_tasks_updated" AFTER UPDATE ON "tasks"
WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE "tasks" SET updated_at=utc_now() WHERE "id"=NEW."id"; END;

CREATE TABLE IF NOT EXISTS "tool_usage" (
    "id" INTEGER PRIMARY KEY AUTOINCREMENT,
    "ts" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    "tool_key" TEXT NOT NULL,
    "agent_key" TEXT,
    "task_id" INTEGER,
    "exploration_id" INTEGER,
    "intent_id" INTEGER,
    "session_id" TEXT
);

CREATE TABLE IF NOT EXISTS "tools" (
    "key" TEXT NOT NULL,
    "system" BOOLEAN NOT NULL DEFAULT true,
    "description" TEXT NOT NULL DEFAULT '',
    "schema" JSON_TEXT NOT NULL DEFAULT '{}' CHECK ("schema" IS NULL OR json_valid("schema")),
    "agents" JSON_TEXT NOT NULL DEFAULT '[]' CHECK ("agents" IS NULL OR json_valid("agents")),
    "enabled" BOOLEAN NOT NULL DEFAULT true,
    "kind" TEXT NOT NULL DEFAULT 'builtin',
    "exec" JSON_TEXT NOT NULL DEFAULT '{}' CHECK ("exec" IS NULL OR json_valid("exec")),
    "deferred" BOOLEAN NOT NULL DEFAULT false,
    "updated_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    PRIMARY KEY (key)
);

CREATE TRIGGER IF NOT EXISTS "trg_tools_updated" AFTER UPDATE ON "tools"
WHEN NEW.updated_at = OLD.updated_at
BEGIN UPDATE "tools" SET updated_at=utc_now() WHERE "key"=NEW."key"; END;

CREATE TABLE IF NOT EXISTS "traffic_evidence_snapshots" (
    "id" TEXT NOT NULL,
    "source_traffic_id" TEXT NOT NULL,
    "captured_at" INTEGER NOT NULL,
    "url" TEXT NOT NULL,
    "method" TEXT NOT NULL,
    "status" INTEGER NOT NULL,
    "content_type" TEXT NOT NULL DEFAULT '',
    "req_head" TEXT NOT NULL,
    "resp_head" TEXT NOT NULL,
    "req_hash" TEXT NOT NULL,
    "resp_hash" TEXT NOT NULL,
    "req_len" INTEGER NOT NULL,
    "resp_len" INTEGER NOT NULL,
    "unreferenced_at" TIMESTAMP,
    "created_at" TIMESTAMP NOT NULL DEFAULT (utc_now()),
    PRIMARY KEY (id)
);

CREATE INDEX IF NOT EXISTS idx_act_latest ON activity (exploration_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_act_main_seg ON activity (exploration_id, main_seg, id) WHERE (worker = 'mainagent');

CREATE INDEX IF NOT EXISTS idx_act_node ON activity (exploration_id, node_id, id);

CREATE INDEX IF NOT EXISTS idx_act_result_usage ON activity (exploration_id) WHERE (kind = 'result');

CREATE INDEX IF NOT EXISTS idx_act_since ON activity (exploration_id, id);

CREATE INDEX IF NOT EXISTS idx_act_tool_call ON activity (exploration_id, tool_use_id, id) WHERE (kind IN ('tool_use', 'tool_result'));

CREATE INDEX IF NOT EXISTS idx_act_worker ON activity (exploration_id, worker, id);

CREATE INDEX IF NOT EXISTS idx_askv_skill ON agent_skill_visibility (skill_name);

CREATE INDEX IF NOT EXISTS idx_agent_triggers_agent ON agent_triggers (agent_key);

CREATE INDEX IF NOT EXISTS idx_vis_resource ON agent_visibility (resource_kind, resource_id);

CREATE INDEX IF NOT EXISTS idx_agents_llm_profile ON agents (llm_profile_id) WHERE (llm_profile_id IS NOT NULL);

CREATE INDEX IF NOT EXISTS idx_asset_intercept_enabled ON asset_intercept_rules (enabled);

CREATE INDEX IF NOT EXISTS idx_av2_company ON assets (company_id) WHERE (company_id IS NOT NULL);

CREATE INDEX IF NOT EXISTS idx_av2_company_type ON assets (company_id, type) WHERE (company_id IS NOT NULL);

CREATE INDEX IF NOT EXISTS idx_av2_domain ON assets (domain) WHERE (domain IS NOT NULL);

CREATE INDEX IF NOT EXISTS idx_av2_ip ON assets (ip) WHERE (ip IS NOT NULL);

CREATE INDEX IF NOT EXISTS idx_av2_last_seen ON assets (last_seen DESC);

CREATE INDEX IF NOT EXISTS idx_av2_root_domain ON assets (root_domain) WHERE (root_domain IS NOT NULL);

CREATE INDEX IF NOT EXISTS idx_av2_type_seen ON assets (type, last_seen DESC);

CREATE UNIQUE INDEX IF NOT EXISTS uq_av2_app_bundle ON assets (bundle_id) WHERE ((type = 'app') AND (bundle_id IS NOT NULL));

CREATE UNIQUE INDEX IF NOT EXISTS uq_av2_app_name ON assets (app_name) WHERE ((type = 'app') AND (bundle_id IS NULL));

CREATE UNIQUE INDEX IF NOT EXISTS uq_av2_endpoint ON assets (url, method) WHERE (type = 'endpoint');

CREATE UNIQUE INDEX IF NOT EXISTS uq_av2_ip ON assets (ip) WHERE (type = 'ip');

CREATE UNIQUE INDEX IF NOT EXISTS uq_av2_root_domain ON assets (domain) WHERE (type = 'root_domain');

CREATE UNIQUE INDEX IF NOT EXISTS uq_av2_service_http ON assets (url) WHERE ((type = 'service') AND (service_type = 'http'));

CREATE UNIQUE INDEX IF NOT EXISTS uq_av2_service_other ON assets (COALESCE(domain, ''), COALESCE(ip, ''), port, service_name) WHERE ((type = 'service') AND (service_type = 'other'));

CREATE UNIQUE INDEX IF NOT EXISTS uq_av2_subdomain ON assets (domain, COALESCE(record_type, '')) WHERE (type = 'subdomain');

CREATE INDEX IF NOT EXISTS idx_companies_nkey ON companies (nkey);

CREATE INDEX IF NOT EXISTS idx_sv2_company ON company_scope (company_id);

CREATE INDEX IF NOT EXISTS idx_sv2_domain ON company_scope (domain) WHERE (kind = 'domain');

CREATE INDEX IF NOT EXISTS idx_sv2_icp ON company_scope (value) WHERE (kind = 'icp');

CREATE UNIQUE INDEX IF NOT EXISTS uq_sv2_value ON company_scope (company_id, kind, value) WHERE (kind IN ('icp', 'keyword'));

CREATE INDEX IF NOT EXISTS idx_conv_act ON conversation_activities (conversation_id, id);

CREATE INDEX IF NOT EXISTS idx_conv_act_tool_call ON conversation_activities (conversation_id, tool_use_id, id) WHERE (kind IN ('tool_use', 'tool_result'));

CREATE INDEX IF NOT EXISTS idx_conversations_llm_profile ON conversations (llm_profile_id) WHERE (llm_profile_id IS NOT NULL);

CREATE INDEX IF NOT EXISTS idx_conversations_pinned ON conversations (pinned_at DESC) WHERE (pinned_at IS NOT NULL);

CREATE INDEX IF NOT EXISTS idx_anchor_asset ON exploration_anchors (asset_id);

CREATE INDEX IF NOT EXISTS idx_expedges_dst ON exploration_edges (dst_id, rel);

CREATE INDEX IF NOT EXISTS idx_expedges_src ON exploration_edges (src_id, rel);

CREATE INDEX IF NOT EXISTS idx_expnodes_frontier ON exploration_nodes (exploration_id, priority DESC) WHERE ((kind = 'intent') AND (state = 'open'));

CREATE INDEX IF NOT EXISTS idx_expnodes_part ON exploration_nodes (exploration_id, kind);

CREATE UNIQUE INDEX IF NOT EXISTS idx_finding_retests_active ON finding_retests (finding_id) WHERE (status IN ('pending', 'running'));

CREATE INDEX IF NOT EXISTS idx_finding_retests_history ON finding_retests (finding_id, id DESC);

CREATE INDEX IF NOT EXISTS idx_finding_traffic_order ON finding_traffic_bindings (finding_id, "position", id);

CREATE INDEX IF NOT EXISTS idx_finding_traffic_snapshot ON finding_traffic_bindings (snapshot_id);

CREATE INDEX IF NOT EXISTS idx_findings_status ON findings (status, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_findings_task ON findings (task_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_findings_time ON findings (created_at DESC);

CREATE INDEX IF NOT EXISTS idx_intercept_pending_status ON intercept_pending (status, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_intercept_pending_task ON intercept_pending (task_id, created_at DESC);

CREATE UNIQUE INDEX IF NOT EXISTS uq_llm_one_default ON llm_profiles (is_default) WHERE is_default;

CREATE INDEX IF NOT EXISTS idx_llm_records_session ON llm_records (session_id);

CREATE INDEX IF NOT EXISTS idx_llm_records_ts ON llm_records (ts);

CREATE INDEX IF NOT EXISTS idx_llm_usage_exp ON llm_usage (exploration_id);

CREATE INDEX IF NOT EXISTS idx_llm_usage_model ON llm_usage (task_id, model);

CREATE INDEX IF NOT EXISTS idx_llm_usage_task ON llm_usage (task_id);

CREATE INDEX IF NOT EXISTS idx_notification_deliveries_batch ON notification_deliveries (batch_id) WHERE (batch_id IS NOT NULL);

CREATE INDEX IF NOT EXISTS idx_notification_deliveries_channel ON notification_deliveries (channel_id, id DESC);

CREATE INDEX IF NOT EXISTS idx_notification_deliveries_due ON notification_deliveries (next_attempt_at) WHERE (state = 'pending');

CREATE INDEX IF NOT EXISTS idx_notification_deliveries_history ON notification_deliveries (id DESC);

CREATE INDEX IF NOT EXISTS idx_notification_events_pending ON notification_events (id) WHERE (NOT fanned_out);

CREATE INDEX IF NOT EXISTS idx_server_logs_id ON server_logs (id DESC);

CREATE UNIQUE INDEX IF NOT EXISTS idx_side_request_running ON side_question_requests (session_key) WHERE (status = 'running');

CREATE INDEX IF NOT EXISTS idx_side_requests_history ON side_question_requests (session_key, ordinal DESC);

CREATE INDEX IF NOT EXISTS idx_side_sessions_conv ON side_question_sessions (conversation_id);

CREATE INDEX IF NOT EXISTS idx_side_sessions_exp ON side_question_sessions (exploration_id);

CREATE INDEX IF NOT EXISTS idx_side_sessions_intent ON side_question_sessions (intent_id);

CREATE INDEX IF NOT EXISTS idx_side_sessions_task ON side_question_sessions (task_id);

CREATE INDEX IF NOT EXISTS idx_skill_usage_skill ON skill_usage (skill, ts DESC);

CREATE INDEX IF NOT EXISTS idx_skill_usage_task ON skill_usage (task_id);

CREATE INDEX IF NOT EXISTS idx_task_archives_archived ON task_archives (archived_at DESC, id DESC);

CREATE INDEX IF NOT EXISTS idx_task_archives_state ON task_archives (state, requested_at, id);

CREATE INDEX IF NOT EXISTS idx_task_asset_links_asset ON task_asset_links (asset_id, task_id);

CREATE INDEX IF NOT EXISTS idx_task_asset_links_node ON task_asset_links (source_node_id) WHERE (source_node_id IS NOT NULL);

CREATE INDEX IF NOT EXISTS idx_task_categories_name ON task_categories (name, id);

CREATE INDEX IF NOT EXISTS idx_task_constraints_exp ON task_constraints (exploration_id);

CREATE INDEX IF NOT EXISTS idx_task_intercept_task ON task_intercept_rules (task_id);

CREATE INDEX IF NOT EXISTS idx_task_llm_profiles_order ON task_llm_profiles (task_id, "position");

CREATE INDEX IF NOT EXISTS idx_task_llm_profiles_profile ON task_llm_profiles (profile_id, task_id);

CREATE INDEX IF NOT EXISTS idx_task_relations_source ON task_relations (source_task_id);

CREATE INDEX IF NOT EXISTS idx_ts_company ON task_scope (company_id) WHERE (kind = 'company');

CREATE INDEX IF NOT EXISTS idx_ts_domain ON task_scope (domain) WHERE (kind IN ('root_domain', 'subdomain'));

CREATE UNIQUE INDEX IF NOT EXISTS uq_task_scope_v2 ON task_scope (task_id, kind, COALESCE(domain, ''), COALESCE((net), ''), COALESCE(company_id, (0)), COALESCE(value, ''));

CREATE INDEX IF NOT EXISTS idx_task_templates_updated ON task_templates (updated_at DESC, id DESC);

CREATE INDEX IF NOT EXISTS idx_tasks_active_llm_profile ON tasks (active_llm_profile_id) WHERE (active_llm_profile_id IS NOT NULL);

CREATE INDEX IF NOT EXISTS idx_tasks_alive ON tasks (created_at DESC) WHERE (deleted_at IS NULL);

CREATE INDEX IF NOT EXISTS idx_tasks_archived ON tasks (archived_at DESC) WHERE (archived_at IS NOT NULL);

CREATE INDEX IF NOT EXISTS idx_tasks_category ON tasks (category_id, created_at DESC) WHERE ((deleted_at IS NULL) AND (category_id IS NOT NULL));

CREATE INDEX IF NOT EXISTS idx_tasks_llm_profile ON tasks (llm_profile_id) WHERE (llm_profile_id IS NOT NULL);

CREATE INDEX IF NOT EXISTS idx_tasks_pinned ON tasks (pinned_at DESC) WHERE ((deleted_at IS NULL) AND (pinned_at IS NOT NULL));

CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks (status) WHERE (deleted_at IS NULL);

CREATE INDEX IF NOT EXISTS idx_tool_usage_task ON tool_usage (task_id);

CREATE INDEX IF NOT EXISTS idx_tool_usage_tool ON tool_usage (tool_key, ts DESC);

CREATE TRIGGER IF NOT EXISTS trg_conversation_retest_delete BEFORE DELETE ON conversations
BEGIN UPDATE finding_retests SET status='stopped',error='复测会话已删除',finished_at=utc_now() WHERE conversation_id=OLD.id AND status IN ('pending','running'); END;

CREATE TRIGGER IF NOT EXISTS trg_assets_task_links_insert AFTER INSERT ON assets
BEGIN
 INSERT INTO task_asset_links(task_id,asset_id,source,source_summary)
 SELECT t.id,NEW.id,'system','任务执行期间自动关联' FROM json_each(NEW.task_ids) requested JOIN tasks t ON t.id=requested.value AND t.deleted_at IS NULL
 WHERE true ON CONFLICT(task_id,asset_id) DO NOTHING;
 DELETE FROM task_asset_links WHERE asset_id=NEW.id AND task_id NOT IN (SELECT value FROM json_each(NEW.task_ids));
END;

CREATE TRIGGER IF NOT EXISTS trg_assets_task_links_update AFTER UPDATE OF task_ids ON assets
BEGIN
 INSERT INTO task_asset_links(task_id,asset_id,source,source_summary)
 SELECT t.id,NEW.id,'system','任务执行期间自动关联' FROM json_each(NEW.task_ids) requested JOIN tasks t ON t.id=requested.value AND t.deleted_at IS NULL
 WHERE true ON CONFLICT(task_id,asset_id) DO NOTHING;
 DELETE FROM task_asset_links WHERE asset_id=NEW.id AND task_id NOT IN (SELECT value FROM json_each(NEW.task_ids));
END;

CREATE TRIGGER IF NOT EXISTS trg_task_asset_links_updated AFTER UPDATE ON task_asset_links
WHEN NEW.updated_at=OLD.updated_at
BEGIN UPDATE task_asset_links SET updated_at=utc_now() WHERE task_id=NEW.task_id AND asset_id=NEW.asset_id; END;
CREATE TRIGGER IF NOT EXISTS trg_task_llm_profiles_updated AFTER UPDATE ON task_llm_profiles
WHEN NEW.updated_at=OLD.updated_at
BEGIN UPDATE task_llm_profiles SET updated_at=utc_now() WHERE task_id=NEW.task_id AND profile_id=NEW.profile_id; END;
