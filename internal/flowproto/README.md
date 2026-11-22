# 原生 Graph Flow 实验

本实验位于 `experiment/graph-flow-tools` 分支，已接入前端编排、SQLite 草稿与发布版本、试运行、节点事件以及按 Agent 绑定的工具注册。调度仍使用 tRPC-Agent-Go 原生 Graph，未替换现有任务调度器。

## 直接运行

在仓库根目录运行，不需要模型 Key、生产数据库或目标网络：

```sh
./scripts/go-netloom.sh go run ./cmd/flowprobe
./scripts/go-netloom.sh go run ./cmd/flowprobe -input '{"target":"http://example.test"}'
./scripts/go-netloom.sh go run ./cmd/flowprobe -trace -dot /tmp/scopeforge-flow.dot
./scripts/go-netloom.sh go test -race ./internal/flowproto ./cmd/flowprobe
```

默认输入走 `accepted` 分支，HTTP 字符串走 `rejected` 分支。`inspect` 是确定性的演示工具，仅检查输入字符串，不发送网络请求。流程文件为 [assessment.json](testdata/assessment.json)，可通过 `-flow` 指定另一份定义。

`-trace` 在 stderr 显示 SDK 原生事件，例如 `graph.node.start`、`graph.node.complete`、`tool.response`；stdout 返回一次最终 JSON。`-dot` 使用编译后原生 Graph 的 `DOT()` 导出真实执行结构。

## 原生能力与适配边界

| 环节 | 实现 |
| --- | --- |
| 状态与消息 | `graph.MessagesStateSchema`、原生 reducer |
| 参数准备／结果收集 | `StateGraph.AddNode` 函数节点 |
| 工具执行 | `StateGraph.AddToolsNode`，使用传入的现有 CallableTool |
| 条件跳转 | `StateGraph.AddConditionalEdges` |
| 调度和取消 | 原生 Graph Executor，由 GraphAgent 创建 |
| 封装为工具 | `agenttool.NewTool`，开启 `WithStreamInner(true)`、`ResponseModeFinalOnly` |
| 父 Agent 调用 | 原生 LLMAgent + Runner，测试中使用确定性模型 |

项目适配层只校验 JSON 定义、映射参数、将工具结果放入状态；没有自己实现流程调度、模型循环或 AgentTool。工具节点由准备参数、原生工具执行、收集结果三个原生节点组成，DOT 中可以看到这些桥接节点。

定义包含名称、版本、输入 Schema、入口和节点。第一版支持 `start`、`tool`、`branch`、`end`，通过 `{"$ref":"input.target"}` 或 `{"$ref":"steps.inspect.text"}` 取值；没有表达式或动态代码生成。已有工具返回 `toolkit.Output` 时，可以读取其 `text` 字段。嵌入 text 中的 HTTP 元信息不会被擅自解析，后续需要单独明确其结构化输出契约。

编译时冻结原始定义；每次调用使用独立运行状态。当前固定 SDK 版本的 AgentTool 在取消时可能返回空结果而不带错误，因此薄适配层在 `Call` 入口和返回时检查调用者 context；没有替换原生执行循环。工具集合由调用方显式提供，生产接入时应传入已按调用者权限和任务范围过滤的工具集合。

## 已验证与未接入

测试覆盖两个条件分支、原生父 Agent 调用、内层事件转发、输入与引用错误、缺失工具、非法图、草稿修改隔离、并发状态隔离、工具失败终止和取消传递。

当前支持开始、工具、条件分支和结束节点。尚未接入 Agent 节点、并行／循环、人工中断恢复和持久化 checkpoint。服务重启后未完成的试运行标记为 interrupted，不自动重放有副作用的工具。单独调用 AgentTool 仍使用 SDK 自带的隔离内存 Runner；试运行记录与节点事件属于业务 SQLite 数据，不代表原生 checkpoint 恢复已经完成。

## 前端试用

构建并启动实验分支后，从侧栏「功能 → 工作流」进入 `/function/flows`。

1. 页面默认载入上述示例，可直接点击「试运行」。HTTPS 输入返回 accepted，HTTP 输入返回 rejected；示例不发网络请求，也不需要配置模型。
2. 在画布拖动节点和连接点，点击节点编辑工具、参数、条件或输出；也可切换 JSON 视图。保存和试运行都会调用后端编译校验，非法图不会执行。
3. 「工具调用」支持 inspect、http_request 和测绘工具。业务请求沿用现有配置和 HTTP 资产规则；可关联任务 ID。试运行最长 5 分钟，支持停止。
4. 「发布为工具」固定当前版本，并绑定填写的 Agent（逗号分隔）。工具名为 flow_<name>，绑定 Agent 下一次运行时可见。修改草稿不会改变已发布版本；依赖工具不在调用者权限集合中时不暴露该 Flow。
5. 运行记录、输入、最终结果与原生节点事件保存于 SQLite，刷新后可从运行记录重新查看。SSE 使用事件 ID 断线补读；HTTP 初始状态不能覆盖已收到的 SSE 终态。

SQLite schema v9 增加 flows、flow_runs、flow_run_events 表，旧数据通过启动迁移保留。前端没有独立执行器，所有运行请求交给后端的原生 GraphAgent/AgentTool。
