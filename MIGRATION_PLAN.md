# 原生 tRPC / SQLite 迁移计划

## 基线与 Git

- 起点：refactor/sqlite-trpc-web；用户已有脚本删除不纳入迁移提交。
- 工作分支：refactor/native-trpc-sqlite。
- 原有未提交差异备份：/tmp/scopeforge-before-trpc.patch。
- 每个可构建阶段独立提交；不提交配置密钥和生成产物。

## 开发顺序与验收

1. Makefile：前端构建、后端构建、内嵌前端发行、开发运行、测试和清理。
2. SQLite：替换配置、schema、专用 SQL、事务并发和测试数据库；业务数据保留持久化。
3. tRPC：原生 model.Model / tool.CallableTool / LLMAgent / Runner，删除 norma 及相关实现；保留业务审批、工具、用量和流事件行为。
4. Session：使用 tRPC inmemory 服务，进程内共享；重启丢弃，不写 SQLite。业务审计与任务数据不作为模型会话恢复来源。
5. Memory / Knowledge：只预留原生依赖注入点，默认关闭；写明数据边界、隔离、导入、索引及删除方案，等待产品决策。
6. 验证：临时 SQLite 上运行数据库测试、原生模型与工具集成测试、全量 Go 测试、前端检查和发行构建。

## Memory / Knowledge 待决策

- Memory：Runner.WithMemoryService 管理服务；明确用户和任务隔离后，再决定可见工具、自动提取/预载策略、保留期和删除接口。不能用长期记忆绕过 session 不落库要求。
- Knowledge：LLMAgent 注入原生 knowledge 服务；导入文档 → 分块 → embedding → 向量索引 → 带来源检索；后续决定 embedding 模型、存储后端、重建流程及授权范围。
- 默认不连接存储、不注册记忆写入或检索工具、不自动上传已有数据。

## 已确认的代码改动范围

### SQLite

- `config/config.go`、`config.example.json`、`.env.example`：改为数据库文件路径及 `ARTEX_DB_PATH`，默认随 data 目录存储；删除 PostgreSQL 连接字段。
- `db/db.go`、`db/schema.sql`：使用已有 modernc SQLite 驱动；建立新 SQLite schema，不把 PostgreSQL 历史 DDL 直接搬入。启用 foreign_keys、busy_timeout、WAL；所有连接都必须应用连接级 PRAGMA。
- `db/assets.go`、`db/tasks.go` 等：数组改 JSON 数组，通过 `json_each` 查询成员；JSON 合并明确区分对象合并和数组去重。不能把 PostgreSQL 数组字面量直接写入 SQLite。
- `db/companies.go`、`db/task_scope.go`：IP/CIDR 使用 Go `net/netip` 判断包含与最长前缀，确保 IPv4、IPv6、非法地址处理一致。
- 锁与队列：删除 advisory lock 和 `FOR UPDATE SKIP LOCKED`；用短事务及 SQLite 单写者机制原子领取，事务前先准备耗时数据，事务内不进行网络调用。验证并发领取只有一位成功、删除和写入不会产生孤儿。
- `db/task_archives*.go`：移除 `row_to_json`、`json_populate_recordset`、sequence 操作，在 Go 中扫描/还原行并绑定列；保留归档、复原、证据版本语义。
- 审核所有独立建表入口，尤其 `db/llm_usage.go`、LLM 记录表初始化；不能只替换主 schema。
- `agent/testmain_test.go`、`server/testmain_test.go`、`db/testmain_test.go`：移除共享 PostgreSQL 和 advisory lock；使用临时目录内 SQLite 文件，数据库初始化失败必须 fail，不能 skip 掩盖迁移错误。
- 现存 PostgreSQL 数据导入不是自动启动行为；如有存量数据需要保留，另行做显式导出/导入与数量、外键、JSON 验证。

### 原生 tRPC

- SDK 已核对版本：`trpc.group/trpc-go/trpc-agent-go v1.12.0`。实现时锁定版本并审查 `go mod tidy` 的最终依赖。
- `agent/provider.go`：直接返回 `model.Model`；OpenAI、Responses 和 Anthropic 的配置与请求验证分别处理。
- `llmpool/pool.go`、`llmrec/llmrec.go`、`server/task_llm.go`：直接消费 `model.Request` / `model.Response`。流开始前可切换供应商，开始输出后不重放请求；同时处理调用错误及 response.Error。
- 所有业务工具直接实现 `tool.CallableTool` / `tool.Declaration`，从业务上下文读取执行环境；删除 Norma 的 ToolContext、ContentBlock、Result、permission 和 UnlockSet 依赖。
- `agent/capture.go` 直接消费 Runner event，保持工具调用 ID、返回值、文本、推理、错误和用量的 Activity 语义，避免增量与最终结果重复计费。
- `guard/guard.go`：改用原生工具回调；保留人工审批与拦截审计，审批未通过不得执行工具。
- `agent/chat.go`、`mainagent.go`、`worker.go`、`planner.go`、`goals.go`：改用 LLMAgent / Runner；取消、超时、调用预算和收尾语义显式测试，不照搬旧 terminal reason。
- `server/assembly.go`、`mcphttp/client.go`：原生工具注册与 MCP 装配；技能发现、加载及访问控制使用原生技能能力，取消旧 SDK 的延迟工具协议。
- 删除 `agent/noa.go` 与旧压缩相关设置/UI，而不是留空实现；上下文压缩改由 tRPC session summary 机制承担。
- 验收搜索：Go 源码、模块依赖、配置/UI、README 中均不应保留 Norma/noa 功能入口；不创建同形 API、type alias 或本地替代模块。

### Session 边界

- 在 manager 生命周期内创建并共享一个 `session/inmemory` 服务；每次 HTTP 请求不能新建服务，否则连续对话会丢上下文。
- Runner 会话标识包含用户与任务/意图归属。对同一 session 的并发请求保留串行执行约束，不同 session 可以并行。
- 删除业务对话或任务时同步删除内存会话；配置合理过期策略并在关闭 manager 时释放服务。
- 停止创建 `transcript.Store`，不从旧 transcript 或 Activity 重建模型上下文。
- `db/side_questions.go` 当前将完整模型请求保存在 `side_question_sessions`，必须迁移到进程内的旁路 checkpoint 服务；不能只把主 Runner 改成 inmemory 就认为 session 已不落库。
- 聊天列表、任务、执行审计与模型 session 是不同的数据用途。当前假设前者仍持久化，完整 checkpoint 与模型会话仅存内存；已向用户询问此边界。

## 检查点状态

- [x] 建立迁移分支并备份用户现有差异。
- [x] Make 后端构建通过。
- [x] 前端类型检查与 7 项测试通过。
- [x] 内嵌前端发行构建验证（`make release`）。
- [x] SQLite 迁移及临时数据库集成测试；当前全量 Go 测试通过。
- [ ] 原生 tRPC 迁移及无旧依赖检查。
- [ ] Session 不落库及生命周期验证。
- [ ] Memory / Knowledge 原生代码注入留位。
- [ ] 全量回归验证。

基线检查：`go test ./...` 在本次运行失败，22 个 server 测试直接因未设置 PostgreSQL 配置失败；其余包通过或跳过数据库相关测试。该结果不能作为迁移后验收通过的证据。

SQLite 实现移植并适配自本机同源项目提交 `3f7e686`，移植过程中保持当前目录布局与现有业务 API；没有引入该项目的其他运行时。
