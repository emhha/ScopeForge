# 原生工作流

位于 `experiment/graph-flow-tools`。前端入口为「功能 → 工作流」，已发布工具位于「系统 → 工具 → 自定义工具」，类型为 workflow。运行调度使用 tRPC-Agent-Go 原生 StateGraph、GraphAgent、AddToolsNode、AddConditionalEdges、AddSubgraphNode、AddJoinEdge 和 AgentTool。

## 已实现的使用方式

- 前端保存草稿、校验、可视化/JSON 编辑、试运行、停止、发布、版本历史、回滚和下架。
- Agent 通过 workflow_catalog 查询当前允许的工具和 DSL，通过 workflow_validate 校验，然后 workflow_run 直接运行内联定义。临时运行不创建工作流或工具，但全过程落盘，可以从运行详情另存为草稿。
- 发布创建不可变版本并在同一事务中注册真实的自定义工具 `flow_<name>`。工具页可管理开关和 Agent 绑定，草稿修改不影响发布版本。
- 顺序、条件、数据转换和并行汇合。工具节点各用独立原生子图，隔离消息和工具响应，汇合保留所有分支结果。
- 手动、临时、Agent 工具调用共用运行记录、原生事件、错误与取消路径。SSE 合并调用树内的子流程事件并支持游标补读，初始 HTTP 快照不能覆盖已经到达的终态。

## 添加 nmap 或其他工具

在自定义工具页创建 command/script/http 工具，或创建 key 为 nmap 的 shell 环境声明。回到工作流后点击「刷新工具目录」，也会在页面重新获得焦点时刷新。

command/script/http 使用原有执行器；shell 环境声明在工作流中有通用 argv/stdin 适配，Agent 的普通工具列表仍把它作为环境说明。无须为新程序修改 Go 代码，也不会为了显示选项运行目标程序。

例如 nmap 节点可填写：

```json
{"argv":["-sV",{"$ref":"input.domain"}]}
```

也可直接选择 workflow_command：

```json
{"program":"nmap","argv":["-sV",{"$ref":"input.domain"}],"parse":"text"}
```

工作流目录动态包含启用的自定义工具、已授权 MCP、领域工具和原生文件工具。按 Agent 校验/执行时过滤绑定；发布时预检绑定 Agent 的直接依赖，节点调用前再次检查开关/绑定。shell/command 继承现有宿主能力，第三方 CLI 不声称具备 http_request 的逐跳资产检查。

## HTTP、正文与命令组合

http_request 仍由同一个 netloom/netrequest 服务发送一次请求并执行 passive。普通对话保留原文本输出；工作流另外得到 data.status_code、headers、body（预览）、body_artifact、complete、passive、exchange_ids 等结构化字段。

body_artifact 在服务仍持有完整采集字节时保存，和是否打开流量捕获无关。32 KiB 本地 JSON 响应经过工件传给 json.parse 的端到端测试已通过。仍遵守采集上限和 complete 标记，不把 16 KiB 预览冒充完整正文。

解析节点：

```json
{"id":"parse","type":"transform","operation":"json.parse","arguments":{"artifact":{"$ref":"steps.fetch.data.body_artifact"}},"next":"result"}
```

命令 stdin 接完整响应：

```json
{"program":"cat","stdin_artifact":{"$ref":"steps.fetch.data.body_artifact.id"}}
```

通用 command 的 argv 逐项引用，stdout/stderr 分开捕获，结构化结果在 data.stdout/data.stderr/data.value，完整 stdout 也保存工件。已有自定义 command 模板在工作流里同样捕获完整输出，避免宿主控制台预览截断。gzip 工件解析显式添加 content_encoding=gzip。缺省拒绝解析不完整工件。

## 定义和引用

支持 start/tool/transform/branch/parallel/join/end，最多 64 节点；保存时检查结构和必填工具参数，运行时验证真实输入、解析后参数和可选输出 Schema。引用支持 `input.domain`、`steps.scan.data.value`，也支持 JSON Pointer `#/steps/scan/data/value/0`。并行节点 branches 填分支入口；join.from 填需要等待的直接前驱，它们的 next 指向该 join。

转换操作包括 json.parse、json.stringify、lines、join、unique、flatten、select、regex.extract、base64.encode/decode、read_text。它们是确定性原生函数节点，没有动态 eval。

完整定义 Schema 由 workflow_catalog 和 /api/flows/catalog 返回。示例 [assessment.json](testdata/assessment.json) 不访问网络；[domain-recon.json](testdata/domain-recon.json) 保留原 subfinder/amass/httpx/ehole 工具链，仍需目标环境安装兼容 CLI。

## 持久化和验证

SQLite v10 保留旧草稿、已发布快照、运行和事件，增加不可变版本、独立临时运行、运行归属、原生事件载荷与工件索引。Agent 只能读取/取消自己所属 Agent、任务和会话的运行，工件限制在同一调用树中，支持子流程文件继续交给父流程工具；不能跨无关运行读取。发布工具的删除/下架不会在重启后自动复活。

```sh
./scripts/go-netloom.sh go test -race -tags sqlite_fts5 ./db ./server ./agent ./internal/flowproto ./internal/flowcli ./netrequest ./toolkit
make test-web
./scripts/go-netloom.sh go run ./cmd/flowprobe -trace
```

测试覆盖动态目录、真实自定义工具注册、固定版本/下架/回滚、Agent 工具流式接口、临时运行归属、HTTP 完整工件、原生并行工具隔离、旧数据迁移及 SSE 实时到达/回放。浏览器验收仅运行确定性 inspect 示例，不执行外部扫描。

长期扩展设计保存在 [DESIGN.md](DESIGN.md)。原生持久化 checkpoint、人工中断恢复、通用循环和独立 Agent 节点不在当前可用节点列表；服务重启后的未完成运行明确为 interrupted，不自动重放有副作用的命令。
