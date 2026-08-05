# threadmill

Threadmill 是一个面向多 Agent 协作的控制平面设计与原型仓库。系统把持久的工作、任务依赖、上下文、验证和合并与临时 Agent session 分开管理。

## 文档阅读顺序

1. [总体架构](./docs/architecture.md)：产品判断、模块边界和端到端流程。
2. [Task Graph](./docs/task-graph.md)：Task Contract、Task Attempt、phase endpoint、edge、blocker 和 stale result。
3. [Task Manager Agent](./docs/task-manager-agent.md)：requirement intake、内容所有权和 graph mutation 契约。
4. [Context Lib](./docs/ctxlib.md)：Context Block、Context Pack、上下文绑定和可替换存取策略。
5. [Agent Runtime](./docs/agent-runtime.md)：CLI adapter、invocation、权限、事件和 artifact。
6. [Workspace / Merge Queue](./docs/workspace-merge.md)：隔离工作区、验证闸门和合并流程。
7. [Scheduler / Budget](./docs/scheduler-budget.md)：worker capacity、预算和调度优先级。
8. [Event / Artifact Store](./docs/event-artifact-store.md)：事实记录、证据和 projection。

## Task Manager Skill

[`skills/task-manager/SKILL.md`](./skills/task-manager/SKILL.md) 是 Task Manager 的可执行编排规则。它只负责 requirement intake 和 Task Graph mutation，不替 planner 选择实现方案，也不执行代码、验证结果或合并候选变更。

## 设计边界

Task Graph 只保存跨时间、跨 Agent 仍然成立的工作义务。phase 内部的工具调用和 Agent 调用由 Agent Runtime 实现，不自动成为新的 Task 或 Graph 节点。

文档和 Skill 目前都是 Draft；它们描述目标契约，不能被理解为现有代码已经完整实现。
