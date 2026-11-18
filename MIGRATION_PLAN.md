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
