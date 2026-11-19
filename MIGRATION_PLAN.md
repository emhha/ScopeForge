# 原生 tRPC / SQLite 迁移说明

## 开发流程与 Git 检查点

1. 在 `refactor/native-trpc-sqlite` 分支实施；初期保留用户原有脚本删除，不把这些删除纳入运行时迁移提交。原始差异备份在 `/tmp/scopeforge-before-trpc.patch`。
2. Make 构建入口与迁移计划：`0c04675`、`7ee58d4`。
3. SQLite 业务数据、配置、归档与并发事务：`cf8dca8`。
4. 原生 tRPC 运行时与内存 session：`bdce1ee`，包含模型、工具、审批、计费和生命周期测试。
5. Make 发布打包、文档与实际二进制启动验收：独立提交。

## 已完成的实现

- [x] Make：默认构建内嵌前端程序；后端、前端、开发、测试、清理入口；复用前端产物交叉编译与 zip 打包。发布工作流使用相同 Make 目标。
- [x] SQLite：删除 PostgreSQL 驱动、DDL 和专有查询；配置为数据库文件。默认 `-data/scopeforge.sqlite`，`SCOPEFORGE_DB_PATH` 优先，配置内相对路径相对配置文件解析。旧 PostgreSQL 配置显式拒绝，已有数据不自动导入。
- [x] 并发与数据：modernc SQLite，WAL、外键、busy_timeout、FULL 同步；读池和单写连接，写事务 `BEGIN IMMEDIATE`。数组使用 JSON，IP/CIDR 使用 `net/netip`，归档在 Go 中投影和还原。数据库测试必须使用临时 SQLite，初始化失败不跳过。
- [x] 原生运行：删除 norma 模块和运行时，不提供兼容层或本地替代模块。角色直接使用原生 `model.Model`、`tool.CallableTool`、`LLMAgent` 和 `Runner`；业务配置与审计适配保持在应用边界。删除旧 noa 后端、API 和 UI 设置。
- [x] 模型与工具：原生 OpenAI Chat、Responses、Anthropic；保留代理、配额识别、输出前重试与供应商切换。输出后不重放。原生 hostexec、file、todo、skills 与 MCP 工具，业务工具实现原生接口。工具回调先补默认参数并校验 schema，再审批，再执行；保留拦截审计和用量统计。
- [x] Session：Manager 共享 `session/inmemory`，TTL 24 小时；Runner 摘要和待办均在内存。摘要通过原生动态 summarizer 使用当前模型，达到上下文窗口约 2/3 时生成。对话/任务删除或归档时清理内存会话，Manager 关闭时释放。
- [x] 旁路问答：完整 snapshot、历史和幂等状态仅保存在进程内；保留 generation、序号、并发清空保护和 TTL。schema v2 删除旧旁路 session 表，归档不再导出或恢复 checkpoint。重启不会从 Activity、聊天记录或旧 transcript 还原上下文。
- [x] Memory / Knowledge：只预留原生类型注入，默认 nil，不创建服务、不注册功能工具、不导入历史数据。

业务任务、聊天展示记录、活动、审批、用量和按设置启用的 LLM 调用审计仍是 SQLite 中的业务数据；它们不充当模型 session 存储，也不作为恢复输入。

SDK 锁定同一上游修订 `90b8c46f000e`：

- `trpc.group/trpc-go/trpc-agent-go v1.13.0-beta.1.0.20261111095122-90b8c46f000e`
- `model/anthropic v1.12.1-0.20261111095122-90b8c46f000e`
- `model/openai/sdkv3 v1.13.0-beta.1.0.20261111095122-90b8c46f000e`

SQLite 实现移植并适配自本机同源项目提交 `3f7e686`；保持当前包布局与业务 API，没有引入其他运行时。所有模型协议的编码、解析和工具循环由上游 SDK 负责。

## Memory 后续具体接入路径（等待产品决策）

代码留位为 `agent.RunConfig.Memory memory.Service`；非 nil 时传给原生 `runner.WithMemoryService`。下一阶段应：

1. 在 Manager 中按配置创建单一 Memory 服务，明确用户、项目、任务的命名空间以及跨任务是否可见；通过各角色构造配置传到 RunConfig，关闭 Manager 时释放。
2. 选择原生后端及持久化位置，单独管理长期记忆；不把完整会话/checkpoint 改为落库。决定显式记忆工具与自动提取哪些启用，以及写入是否需要用户确认。
3. 明确注入方式、条数与 token 预算、冲突/去重规则、保留期；按用户操作提供查看、修正和删除。
4. 添加隔离、删除、跨任务泄漏、服务故障及未启用时无存储副作用的测试，再开放配置/UI。

当前不选择存储后端、不自动提取或预载、不安装记忆工具。

## Knowledge 后续具体接入路径（等待产品决策）

代码留位为 `agent.RunConfig.Knowledge knowledge.Knowledge`；非 nil 时传给原生 `llmagent.WithKnowledge`。下一阶段应：

1. 确定知识来源、文件格式/大小、用户与项目权限、embedding 模型及向量存储；在 Manager 管理服务及后台导入作业，按角色授权注入。
2. 构建「导入 → 内容提取 → 分块 → embedding → 建索引」管道，用内容哈希避免重复；保留来源、版本、导入状态与错误，支持增量更新和重建。
3. 使用原生检索能力，查询先过滤授权命名空间，再按相关度和 token 预算返回片段；回复提供来源，约束知识内容不能覆盖系统指令。
4. 删除源文档时删除分块/向量；明确任务归档、用户删除和 embedding 更换时的清理策略。验证权限隔离、来源引用、重建一致性和故障回退后再开放 UI。

当前不创建向量库、不调用 embedding、不自动上传业务数据、不挂载知识检索工具。

## 验收方式

- 全量 Go 测试、`go vet ./...`、前端类型检查与 Node 测试。
- 原生 Runner → 工具 → 审批集成，模型三协议/重试/用量与取消，原生命令执行和服务端搜索声明的本地 HTTP fixture。
- SQLite 并发领取、归档恢复、外键清理；进程重开不保留 session，以及旧归档不恢复旁路 session。
- agent/server/llmpool/llmrec/sidequestion/db/mcphttp 的 race 检查。
- `make release`、`make package` 与实际二进制健康检查、内嵌首页和 SQLite schema 检查；交叉编译检查发布目标。
- 以上使用本地 fixture 和临时数据；真实供应商联网调用不属于已验证范围。

原始基线因缺少 PostgreSQL 配置而失败的测试不作为迁移验收依据。

## 最终验收结果（2026-11-18）

- `go test ./...`：全部通过。
- `go vet ./...`：通过。
- `go test -race ./agent ./server ./llmpool ./llmrec ./sidequestion ./db ./mcphttp`：全部通过。
- `make test-web`：类型检查与 7 项测试通过。
- `make release`、`make build-embedded VERSION=dev`、`make package VERSION=dev`：通过。
- Linux amd64 实际启动：内嵌首页、health、管理员初始化、鉴权 API、重启后登录及正常退出全部通过；SQLite header、schema v2、完整性和无 session 表检查通过。
- linux/arm64、darwin/amd64、darwin/arm64、windows/amd64：`CGO_ENABLED=0` 内嵌前端交叉编译通过。其他平台未在本机执行运行验证。
- 代码及模块中无旧 SDK 依赖或运行入口；配置文件不提交，生成物不提交，初期原有脚本删除保持未暂存；后续集成提交保留并提交这些删除。

## 网络工具与命名收尾（2026-11-19）

- `3e7b214` 保存 netloom 原生 HTTP、MITM 与 FOFA/Hunter/Quake 测绘工具、测绘配置界面，以及每 Agent LLM 选择、设置项清理和 dist 构建规则。
- 正式模块路径改为 `github.com/emhha/scopeforge`；命令为 `cmd/scopeforge`，产物为 `dist/scopeforge`；环境变量统一使用 `SCOPEFORGE_`，管理员用户名为 `ScopeForge`。旧环境变量、登录 Cookie 和命令不提供别名，部署时应同步更新并重新登录。
- `make build` 生成当前系统产物；`make build all` 生成 Linux、Windows、macOS 的 amd64/arm64 六种内嵌前端产物。
- netloom 获取方式、证书、工具与设置边界详见 [NETLOOM.md](NETLOOM.md)。内存 session 和 Memory/Knowledge 留位策略保持本说明中的定义。
- main 分支的 Reasonix 迁移与当前原生 tRPC 方案冲突。集成采用当前经测试的 tRPC/SQLite/netloom 树，保留双方 Git 历史；不带回 Reasonix vendor、运行时兼容层或会话文件持久化。

### 收尾验证

- 全量 `make test`、`go vet ./...`、依赖校验通过；traffic、netrequest、discovery、server 的竞态检查通过。
- `make build` 与 `make build all` 通过，生成当前系统程序和六种跨平台程序；非 Linux 平台仅验证交叉编译。
- 实际 Linux 程序验证内嵌 ScopeForge 界面、初始化、管理员登录、测绘配置鉴权、SQLite 完整性、重启后登录和正常退出。
- 原生 netloom 本地集成覆盖 HTTP/HTTPS、CA 重载、上游代理、重定向、压缩、取消、流量证据与三个测绘源；未使用真实供应商 Key 发起计费查询。
- 浏览器验证每 Agent 独立模型选择、保存失败回退、编辑器同步，以及测绘 Key 保存/保留/清除；当前界面与 13 张产品截图使用 ScopeForge 标识。
