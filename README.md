# threadmill

Threadmill 是一个面向多 Agent 协作的控制平面设计与原型仓库。系统把持久的工作、任务依赖、上下文、验证和合并与临时 Agent session 分开管理。

## 编排模型回答的问题

这组文档集中回答四个问题：

1. 为什么 Task 必须独立于 agent session 存在；
2. 为什么需要区分持久的 Coordination Graph 和临时的 Execution Graph；
3. `plan -> execute -> verify` 各自固定什么责任，以及 phase 内部如何递归使用执行结构；
4. Task Manager 怎样判断拆分粒度、选择 edge endpoint，并处理失败与过期结果。

当前 MVP 将持久协调关系统一落在 Task Graph 中；Execution Graph 的语义保留在设计基线和领域语言中，但不作为第二个持久图或独立存储实现。

## 文档阅读顺序

1. [设计基线](./docs/design-rationale.md)：工作为什么不属于 agent session，以及模块各自拥有什么决定。
2. [领域语言](./docs/CONTEXT.md)：requirement、task、attempt、invocation、Coordination Graph 和 Execution Graph 的词义边界。
3. [总体架构](./docs/architecture.md)：产品判断、模块边界和端到端流程。
4. [Task Graph](./docs/task-graph.md)：Task Contract、Task Attempt、phase endpoint、edge、blocker 和 stale result。
5. [Task Manager Agent](./docs/task-manager-agent.md)：requirement intake、内容所有权和 graph mutation 契约。
6. [Context Lib](./docs/ctxlib.md)：Context Block、Context Pack、上下文绑定和可替换存取策略。
7. [Agent Runtime](./docs/agent-runtime.md)：CLI adapter、invocation、权限、事件和 artifact。
8. [Workspace / Merge Queue](./docs/workspace-merge.md)：隔离工作区、验证闸门和合并流程。
9. [Scheduler / Budget](./docs/scheduler-budget.md)：worker capacity、预算和调度优先级。
10. [Event / Artifact Store](./docs/event-artifact-store.md)：事实记录、证据和 projection。

## Task Manager Skill

[`skills/task-manager/SKILL.md`](./skills/task-manager/SKILL.md) 是 Task Manager 的可执行编排规则。它只负责 requirement intake 和 Task Graph mutation，不替 planner 选择实现方案，也不执行代码、验证结果或合并候选变更。

## 文件布局

与现有文档同名的设计内容已经直接整合进对应的 `docs/` 文件；原提案中没有对应现有文档的 `CONTEXT.md` 和 `design-rationale.md` 则以原文件名放在仓库外层 `docs/`。Skill 保持在仓库外层 `skills/`，不与普通设计文档混合。

## 设计边界

Task Graph 只保存跨时间、跨 Agent 仍然成立的工作义务。phase 内部的工具调用和 Agent 调用由 Agent Runtime 实现，不自动成为新的 Task 或持久 Graph 节点。

文档和 Skill 目前都是 Draft；它们描述目标契约，不能被理解为现有代码已经完整实现。
