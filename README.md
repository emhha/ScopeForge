# PenForge

安全渗透智能体平台 —— 博取 [Tsec-Hackathon](https://github.com/Yeti-791/Tsec-Hackathon) 优秀项目之长，以 [deepseek-reasonix](https://github.com/deepseek-ai/reasonix) 为底座实现。

## 简介

PenForge 是一个自主渗透测试智能体平台，面向 CTF/靶场夺旗与授权渗透场景，由 Agent 自主完成「侦察 → 渗透利用 → 提交 flag → 报告生成」的完整闭环，人工只做授权、审批与干预。

- **底座**：迁移自 deepseek-reasonix 开源框架（Provider / Conversation / Tool / Skill / MCP / Memory / Planner / Executor 等核心能力），与其同栈（Go）。
- **参考**：借鉴两届腾讯云智能渗透黑客松（Tsec-Hackathon）获奖团队的开源方案，如 Cairn（黑板 + 无角色 Worker）、BreachWeave（Manager/Solver/Observer 三层解耦）、For Future（YAML 声明式约束）等。
- **核心思路**：把「流程」交给模型、把「边界」收回代码 —— 以约束平面 + 黑板 + 无角色 Worker 调度，而非硬编码流程或角色分工。

## 技术栈

Go（chi 路由，单二进制） · SQLite（WAL） · Vue3 + TypeScript + Vite（`go:embed` 打包） · OpenAI 兼容多 Provider · Docker 沙箱

## 快速开始

```bash
# 构建（前端 + Go 二进制）
./build.sh

# 启动服务（首次启动自动生成 ~/.penforge/penforge.yaml 与 .env）
./bin/penforge serve


# 环境自检 / 离线评测
./bin/penforge doctor
./bin/penforge bench
```

部署见 `deploy/README.md`。

## 免责声明

本工具仅用于授权测试与 CTF/靶场学习交流，禁止用于未授权攻击及任何非法活动。
