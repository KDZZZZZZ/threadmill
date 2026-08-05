# Threadmill Task Orchestration Model

这个目录是一组可独立评审的设计提案，不替换仓库现有 `docs/`。它集中回答四个问题：

1. 为什么 Task 必须独立于 agent session 存在；
2. 为什么 Task Graph 要区分持久的 Coordination Graph 和临时的 Execution Graph；
3. `plan -> execute -> verify` 各自固定什么责任，以及 phase 内部如何递归使用 subgraph；
4. Task Manager 怎样判断拆分粒度、选择 edge endpoint，并处理失败与过期结果。

## 阅读顺序

1. [设计基线](./docs/design-rationale.md)：先看整个工作模型和模块分权。
2. [领域语言](./CONTEXT.md)：确认 requirement、task、attempt、invocation 和 thread 不是同义词。
3. [Task Graph](./docs/task-graph.md)：查看两层 graph、phase endpoint、edge 和 task boundary。
4. [Task Manager Agent](./docs/task-manager-agent.md)：查看 requirement 的内容所有权和 graph mutation 数据契约。
5. [`task-manager` Skill](./skills/task-manager/SKILL.md)：可直接注入 Manager 的编排步骤和拒绝规则。
6. [Context Lib](./docs/ctxlib.md)：查看 Context Pack 如何绑定 contract、attempt、phase、role 和 input revision。

## 目录边界

这里的文件目前是设计候选。确认语义后，可以逐项合入仓库主文档和实现；在此之前，不应让现有代码假装已经实现这些契约。
