# 原生工作流

位于 `experiment/graph-flow-tools`。前端入口为「功能 → 工作流」，已发布工具位于「系统 → 工具 → 自定义工具」，类型为 workflow。运行调度使用 tRPC-Agent-Go 原生 StateGraph、GraphAgent、AddToolsNode、AddConditionalEdges、AddSubgraphNode、AddJoinEdge 和 AgentTool。

## 已实现的使用方式

- 前端保存草稿、校验、可视化/JSON 编辑、试运行、停止、发布、版本历史、回滚和下架。
- Agent 通过 workflow_catalog 查询当前允许的工具和 DSL，通过 workflow_validate 校验，然后 workflow_run 直接运行内联定义。临时运行不创建工作流或工具，但全过程落盘，可以从运行详情另存为草稿。
- 发布创建不可变版本并在同一事务中注册真实的自定义工具 `flow_<name>`。工具页可管理开关和 Agent 绑定，草稿修改不影响发布版本。
- 顺序、条件、数据转换和并行汇合。工具节点各用独立原生子图，隔离消息和工具响应，汇合保留所有分支结果。
- 手动、临时、Agent 工具调用共用运行记录、原生事件、错误与取消路径。SSE 合并调用树内的子流程事件并支持游标补读，初始 HTTP 快照不能覆盖已经到达的终态。
- 关联任务的工作流跟随 Engine 的暂停/删除取消，保留调用者上下文，并计入任务 drain。暂停、排队或删除中的任务不接收新工作流；取消后的命令不自动重放。服务启动时将遗留运行统一标记 interrupted。

## 添加 nmap 或其他工具

**所有执行都使用同一个「工具调用」节点，不需要先注册命令。** 添加节点后在「工具」下拉框选择 Bash，即可输入 shell 命令；也可选择通用的 workflow_command 逐项填写程序参数：

- **程序 + 参数**（`workflow_command`）：直接填写已安装的程序名，每个 argv 单独设置固定值或数据引用；可接 stdin，输出解析为文本、按行数组或 JSON。域名作为参数传递，无须 shell 引号。
- **Bash / Shell**（`Bash`）：直接输入命令、管道或多行脚本，复用现有原生宿主执行器。结果为 `steps.<id>.data.output` / `data.exit_code`，编辑器和 Agent 调用使用相同字段。命令中的 `$ref` 文本不做字符串插值；动态目标优先通过 argv/stdin 传递。

例如，无须创建 nmap 工具即可填写：

```json
{"program":"nmap","argv":["-sV",{"$ref":"input.domain"}],"parse":"text"}
```

已有 shell 命令可直接使用 Bash：

```json
{"command":"printf 'hello\\n' | sort -u"}
```

当需要共享固定命令模板、维护独立说明或绑定 Agent 时，再到自定义工具页创建 command/script/http 工具，或创建 key 为 nmap 的 shell 环境声明。回到工作流后点击「刷新工具目录」，也会在页面重新获得焦点时刷新。command/script/http 使用原有执行器；shell 环境声明在工作流中有通用 argv/stdin 适配，Agent 的普通工具列表仍把它作为环境说明。第三方程序需安装在 ScopeForge 的服务运行环境中，注册工具不会安装程序。

工作流目录动态包含启用的自定义工具、已授权 MCP、领域工具和原生文件工具。按 Agent 校验/执行时过滤绑定；发布时预检绑定 Agent 的直接依赖，节点调用前再次检查开关/绑定。shell/command 继承现有宿主能力，第三方 CLI 不声称具备 http_request 的逐跳资产检查。

subfinder、amass、ProjectDiscovery httpx 和 ehole 不自动注册为工具，也不探测当前系统是否安装。需要时通过 Bash 调用已安装程序；不再维护各 CLI 的专用 Go 适配器。原先使用同名内置工具或 merge_subdomains 的草稿/发布定义需改为 Bash/通用命令和转换节点，或由用户显式创建同名自定义工具。

桌面端画布和参数面板保持等高，长参数在右侧面板内部滚动，不再把画布所在行撑出下方空白；窄屏时面板在画布下方独立滚动。

## 同一域名并行调用多个工具

点击「同域名并行模板」载入可运行示例：开始 → 并行分叉 → 两个命令 → 等待汇合 → 结束。示例仅用 printf 回显域名，可先验证数据传递，再换成目标环境中已安装的 CLI。

1. 在流程输入 Schema 中声明 `domain`，试运行输入填写 `{"domain":"example.com"}`。
2. 从分叉右侧同一个输出点，分别拖线到多个工具。也可选中分叉，在侧栏选择目标后点击「连接」。新增、删除分支不会改变剩余输出端口或连线的身份。
3. 在每个命令的域名参数上选择「引用」，填写或选择 `input.domain`。连线决定执行顺序，引用决定输入数据；连上分叉并不会自动猜测工具的域名参数。
4. 将各分支的最后一步连入同一个「等待汇合」，等待列表由连线自动维护。汇合后可以引用各分支的结果，例如 `steps.dns.data.value` 和 `steps.http.data.value`。
5. 拖动现有连线末端可重连；选中线后按 Delete/Backspace 或在侧栏点击「断开」。点击「整理布局」按依赖层次分开分支。

并行分支必须先在明确的汇合节点汇合，之后再连接公共步骤或结束节点。重复入口、提前结束、汇合前共享执行节点和互斥条件路径的 all-of 汇合会在校验阶段拒绝，避免重复执行或等待永不完成。嵌套分叉/汇合仍然可用。已有定义不自动改写；不满足这些约束的旧定义需补齐汇合后再保存或运行。

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

通用命令与自定义 command 的 stdout/stderr 合计预算为 32 MiB。运行中每 25 ms 检查重定向文件大小，发现超限后取消原生宿主命令；短时突发写入可能超过预算，因此读取仍先检查文件大小，再以剩余预算加一字节为硬上限读取。超限按失败结束，不把截断结果作为完整工件，也不执行下游节点。这是输出捕获限制，不是操作系统磁盘配额。

## 定义和引用

支持 start/tool/transform/branch/parallel/join/end，最多 64 节点；保存时检查结构和必填工具参数，运行时验证真实输入、解析后参数和可选输出 Schema。引用支持 `input.domain`、`steps.scan.data.value`，也支持 JSON Pointer `#/steps/scan/data/value/0`。并行节点 branches 填分支入口；join.from 填需要等待的直接前驱，它们的 next 指向该 join。

转换操作包括 json.parse、json.stringify、lines、join、unique、flatten、select、regex.extract、base64.encode/decode、read_text。它们是确定性原生函数节点，没有动态 eval。

完整定义 Schema 由 workflow_catalog 和 /api/flows/catalog 返回。示例 [assessment.json](testdata/assessment.json) 不访问网络；[domain-recon.json](testdata/domain-recon.json) 通过通用命令调用 Bash 串联可选 CLI，合并去重使用转换节点，目标环境需自行安装兼容 CLI。校验示例不运行命令；实际命令兼容性与第三方版本仍需单独验证。

## 持久化和验证

SQLite v10 保留旧草稿、已发布快照、运行和事件，增加不可变版本、独立临时运行、运行归属、原生事件载荷与工件索引。Agent 只能读取/取消自己所属 Agent、任务和会话的运行，工件限制在同一调用树中，支持子流程文件继续交给父流程工具；不能跨无关运行读取。发布工具的删除/下架不会在重启后自动复活。

删除任务前先取消并等待关联工作流收尾，再在同一事务中清理该任务及子调用的运行、事件和工件索引；保留可复用的工作流定义、发布版本及其他任务/独立运行。勾选「删除文件」时，将工件与任务文件一样暂存，数据库删除失败时恢复，提交后才清除。未勾选时保留本地文件字节，不保留任务的运行索引。新运行的入库检查任务是否仍存在，避免删除后的迟到写入。

```sh
./scripts/go-netloom.sh go test -race -tags sqlite_fts5 ./db ./server ./agent ./internal/flowproto ./netrequest ./toolkit
make test-web
(cd web && node src/lib/flow-editor-browser.test.mjs)
./scripts/go-netloom.sh go run ./cmd/flowprobe -trace
```

测试覆盖动态目录、真实自定义工具注册、固定版本/下架/回滚、Agent 工具流式接口、临时运行归属、HTTP 完整工件、原生并行工具隔离、旧数据迁移及 SSE 实时到达/回放。浏览器验收仅运行确定性 inspect 示例，不执行外部扫描。

编辑器回归还覆盖从空分叉实际拖出两条可见 SVG 连线、键盘删线、侧栏连接、直接 Bash 参数、同域名引用及汇合等待列表同步；运行测试使用本地 printf/cat 验证共享输入、shell 元字符按参数原样传递、Bash 输出契约及运行落盘，不执行外部扫描。

### 本轮设计取舍

参考 [Node-RED 连线交互](https://nodered.org/docs/user-guide/editor/workspace/wires) 的单端口多连线、选择和删除操作，以及 [n8n Execute Command](https://docs.n8n.io/integrations/builtin/core-nodes/n8n-nodes-base.executecommand/) 的直接宿主命令入口。ScopeForge 保留原生 tRPC Graph 调度、显式 `$ref`、独立分支结果、汇合、权限/审批和运行记录；不引入另一套引擎，也不扩展为通用 SaaS 集成平台。任意循环、自动副作用重试、分布式队列和 checkpoint 恢复仍不在本轮范围。

长期扩展设计保存在 [DESIGN.md](DESIGN.md)。原生持久化 checkpoint、人工中断恢复、通用循环和独立 Agent 节点不在当前可用节点列表；服务重启后的未完成运行明确为 interrupted，不自动重放有副作用的命令。
