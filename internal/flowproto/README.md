# 原生 Graph Flow 实验

本原型只验证“声明式流程 → 原生 GraphAgent → 原生 AgentTool → 父 Agent 调用”，位于 `experiment/graph-flow-tools` 分支。生产服务未注册实验工具，没有修改现有任务调度和前端。

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

这个实验尚未接入：前端画布、生产 API、发布／版本存储、Agent 节点、并行／循环、人工中断恢复、持久化 checkpoint、生产 Activity/SSE。单独调用 AgentTool 使用 SDK 自带的隔离内存 Runner；这不代表生产恢复能力已经完成。

下一步先将一个真实业务工具及节点事件适配到现有权限与 Activity 通道，再围绕同一份 JSON 定义制作最小编辑页面。发布版本和执行记录需分开保存；不应把实验中的示例工具直接注册进生产目录。
