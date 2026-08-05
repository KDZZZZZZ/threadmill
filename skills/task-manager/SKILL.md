---
name: task-manager
description: 把人类需求和运行中的新发现编排为 Threadmill Task Contract 与 Task Graph 变更，但不替 planner 发明实现方案。
---

# Task Manager

这个 Skill 只用于 requirement intake 和 Task Graph 变更。不要用它实现代码、编写执行计划、运行验证或合并候选变更。

执行 graph mutation 前读取仓库的 [Task Graph 设计](../../docs/task-graph.md) 和 [Task Manager Agent 详细设计](../../docs/task-manager-agent.md)。如果两者冲突，停止写入并报告冲突，不能用 prompt 中的临时解释覆盖仓库契约。

## 需要先取得的信息

修改 graph 前必须取得：

- 原始 requirement；如果来自 agent，还必须有 `client_ref`；
- 如果请求来自运行中的 task，取得当前 Task Contract 和来源 phase；
- 相关 graph 邻域，包括 endpoint 状态和已有 blocker；
- acceptance intent 和 evidence refs；
- 当前 graph 为什么不足以表达这项工作的说明。

agent requirement 缺少验收意图或证据时返回 `needs_fix`，不要自行补写。

## 判断是否需要新 task

默认把工作留在当前 phase 的内部执行步骤。只有具备下列至少一项，才建立持久 task：

1. 可以独立验收；
2. 可以独立失败或重试；
3. 需要跨时间等待外部输入或人工决定；
4. 需要不同权限、工作区或 owner；
5. 产物会被其他 task 直接依赖；
6. 生命周期超过当前 phase invocation。

tool call、文件读取、局部摘要以及同一已批准计划中的连续步骤，不会因为“看得见”就自动成为 task。

## 保持内容所有权

处理人类输入时，可以澄清语言并提炼可测的 acceptance criteria，但必须保留原始 requirement 作为 provenance。

处理 agent requirement 时，原样保留 `title`、`description`、`acceptance_intent`、`declared_scope`、`client_ref` 和 evidence refs。可以拒绝、要求补全或增加 graph 关系；不得静默改写或合并内容。

实现选择属于 `plan`。requirement 中出现的方案只能作为 hint；只有 requester 明确将其声明为硬约束时，才写入 contract。

## 把 edge 放在真正需要结果的位置

每条 edge 必须有 source endpoint、target endpoint、控制条件和需要传递的数据。真正的依赖只发生在一个 phase 时，不要写成 task 级笼统依赖。

选择最早真正需要 source 结果的 target endpoint：

- A 的规划依赖 B 的已验证事实：`B.verify -> A.plan`；
- A 可以先规划，但实施需要 B：`B.verify -> A.execute`；
- A 可以先实施，但验收必须包含 B：`B.verify -> A.verify`；
- 修改前需要人工授权：`decision.approved(plan_revision) -> A.execute`；
- 验证后的输入 revision 发生变化：使旧验证失效，为同一 task 建立新 attempt。

每条 edge 都必须回答：

```text
它阻止哪个 endpoint？
哪个结果会解除阻止？
什么 evidence 或 message 沿 edge 传递？
条件为 false 或结果过期时发生什么？
```

任何一项答不出来，就不要写 edge。

## 扩展 task，但不要过度拆分

phase 内部可以有多个 Agent 调用、工具调用和连续步骤，但这些步骤默认属于当前 phase 的实现细节。只有内部工作通过前面的 task boundary 检查，才把它提升为持久 task。

运行中的 planner 发现新的持久工作时：

1. 用 `client_ref` 幂等登记 requirement；
2. 创建 Task Contract 和标准 phase endpoint；
3. 把 edge 连到真正消费新结果的 endpoint；
4. 只阻塞来源 task 中受影响的 endpoint；
5. 让 Scheduler 重新计算 runnable endpoint。

graph expansion 成功不等于来源 task 完成。

## 处理失败

- 局部实现或验证失败：为同一 task 创建新 attempt；
- Task Contract 不完整或自相矛盾：阻塞受影响 endpoint，请求澄清或重新立约；
- verify 暴露出独立工作：登记新 task，并连到消费其结果的 endpoint；
- candidate 相对新 revision 已过期：使旧验证失效，按影响重新 verify 或重新 plan；
- 高风险决定缺少权限：创建或关联 human decision endpoint，不得推断已经批准。

重试不是新 task。Task Contract 发生变化时，可以创建替代 task 或显式版本，但不得静默修改已经接受的验收条件。

## Mutation result

返回结构化结果，至少包含：

- decision：`register`、`needs_fix`、`propose_change`、`link_related`、`compile_graph` 或 `reject`；
- 原样回显的 `client_ref`；
- 创建或关联的 task ID；
- 新增的 endpoint、edge 和 blocker；
- 每项 graph 变更的原因；
- evidence refs；
- runnable 状态可能改变的 endpoint 集合。

## 必须拒绝的模式

- 每个 agent 或 tool call 建一个 task；
- 只在 prompt 中描述依赖，不写 graph；
- 只有 `execute` 或 `verify` 受影响，却阻塞整个 task；
- 为表达 parent/child 所有权而制造环；
- 把 worker summary 当作验证证据；
- 每次 attempt 失败都创建新 task；
- acceptance 和 merge 条件未满足就标记 done；
- 为迁就某个实现方案而修改 graph 内容。

## 写入前检查

1. 每个 task 是否只有一个可测的 contract？
2. 每条新 edge 是否落在最早真实需要结果的位置？
3. 是否把没有独立生命周期的执行细节错误提升成 task？
4. agent-originated 内容是否保持原样？
5. failure 和 stale-result 路径是否明确？
6. 杀掉所有 agent 后，这次 mutation 表达的义务是否仍然存在？
