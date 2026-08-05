# Task Graph 设计

版本：v0.5
状态：Draft

Task Graph 保存未完成工作的因果关系。它不是 agent 通信图，也不保存一次运行中的每个工具调用。为了避免把持久协调和临时执行混成一层，本文分别使用 **Coordination Graph** 与 **Execution Graph**。

设计动机和术语边界见[设计基线](./design-rationale.md)。Task Manager 的实际编排规则见 [`skills/task-manager/SKILL.md`](../skills/task-manager/SKILL.md)。

## 1. 两层 graph

### Coordination Graph

Coordination Graph 是持久层。它保存 Task、Phase Endpoint、依赖、blocker 和 decision endpoint。agent 退出后，这一层仍然足以说明哪些工作尚未完成以及为什么不能继续。

Task Manager Agent 是唯一写入者。Scheduler、Runtime、worker agent 和 Merge Queue 只能读取，或提交带证据的 mutation request。

### Execution Graph

Execution Graph 是一次 phase invocation 的运行结构，由 Scheduler 根据当前可运行 endpoint 物化。节点执行体可以是：

- Agent Runtime invocation；
- 确定性 tool；
- 另一个 execution subgraph。

Execution Graph 可以递归，但通常不持久保存为项目任务。只有获得独立契约和生命周期的内部工作才会被 Task Manager 提升为 Task，写回 Coordination Graph。

## 2. 持久对象

### Task

Task 是一个 Task Contract 的持久工作身份。它不区分 root、child 或 subtask 类型；分解、替代和依赖都由边表达。

### Task Attempt

Attempt 是对同一 Task Contract 的一次尝试。验证失败、执行崩溃或输入 revision 过期通常产生新 attempt，而不是新 task。

### Phase Endpoint

每个 task 暴露以下 endpoint：

```text
prepare -> plan -> execute -> verify -> done
```

endpoint 是编排锚点，不是 agent 进程状态。

作为 source 时，endpoint 表示该 phase 已完成并产出了信号与 evidence；作为 target 时，入边参与计算该 phase 是否可以启动。`B.verify -> A.execute` 因而表示“B 的 verify 结果参与激活 A 的 execute”，不是说 A 已经执行完成。

| Endpoint | 满足条件 |
| --- | --- |
| `prepare` | Task Contract、输入基线、权限和必要上下文可用。 |
| `plan` | 影响面、依赖、权限需求和验证方法已经声明。 |
| `execute` | 候选交付及其观察证据已经产生。 |
| `verify` | 候选在明确 revision 上满足 Task Contract。 |
| `done` | 验收、依赖、合入或非代码交付条件全部成立。 |

`done` 不启动 agent。它是 graph 根据已满足条件得出的结论。

## 3. Edge 同时表达控制和数据

一条可执行的 edge 至少包含：

```go
type CoordinationEdge struct {
	From      PhaseEndpointRef      `json:"from"`
	To        PhaseEndpointRef      `json:"to"`
	Condition SignalCondition       `json:"condition"`
	Data      []ArtifactOrMessageRef `json:"data,omitempty"`
	OnFalse   EdgeFailurePolicy     `json:"on_false"`
}
```

`Condition` 决定目标 endpoint 是否可运行，`Data` 说明目标运行时必须消费哪些结果。只写 `A depends_on B` 不够，因为它没有说明 A 的哪个阶段真正需要 B，也没有说明 B 的什么结果构成依赖。

Manager 应把边连到最早真实需要结果的位置：

```text
B.verify -> A.plan     A 的方案依赖 B 的已验证结论
B.verify -> A.execute  A 可以先规划，但实施必须等待 B
B.verify -> A.verify   A 可以先实施，但最终验收必须包含 B
```

过早的 edge 会损失并发；过晚的 edge 会让 agent 在无效前提上工作。

## 4. task 的默认生命周期

一次正常 attempt 的控制路径是：

```text
prepare -> plan -> execute -> verify
```

结果分三类：

```text
verify passed 且交付条件满足 -> done
verify failed 但契约仍成立   -> 同一 task 的新 attempt
verify 发现独立前置工作      -> 新 task + 精确 endpoint edge
```

验证失败不是 `verify -> done` 的另一条正常边。失败会使旧 attempt 终止，并由 Task Manager 根据证据决定重新 plan、重新 execute、等待人类决定或新增 task。

## 5. phase 内部的递归编排

固定 endpoint 不限制 phase 内部的运行复杂度。例如一个 `plan` endpoint 可以被物化为：

```text
repo-inspection(tool)
  -> impact-analysis(planner)
  -> plan-schema-check(tool)
  -> requirement-intake(task-manager)
```

其中任何节点也可以调用 subgraph。它们仍属于同一个 phase，除非内部工作需要独立验收、独立重试、跨时间等待、不同权限边界，或者其结果要被其他 task 直接依赖。

如果 `impact-analysis` 发现需要单独完成配置迁移，Task Manager 可以创建 Task B，并只阻塞真正消费 B 结果的 endpoint：

```text
A.plan 产生 requirement B
Task Manager 创建 B.prepare -> B.plan -> B.execute -> B.verify
B.verify --passed + evidence--> A.execute
```

这时 A.plan 的一次运行扩展了 Coordination Graph，但它没有直接创建 task 或 edge，也没有因为“成功拆解”而让 A 完成。

## 6. graph mutation 链路

```text
Human / planner / executor / verifier
  -> requirement or graph-mutation request + evidence
  -> Agent Runtime(role=task_manager, graph_write)
  -> Task Manager reads current graph
  -> register / reject / link / compile graph
  -> Event Log records the mutation
  -> Scheduler recomputes runnable endpoints
```

agent-originated requirement 必须带稳定 `client_ref`。相同 `client_ref` 重放时必须得到同一登记结果，避免重试造成重复 task。

Task Manager 可以增加全局关系，但不能改写 agent-originated requirement 的内容。它也不能把自己的实现偏好写进 Task Contract；“怎么做”属于 plan。

## 7. blocker、decision 和 stale result

blocker 必须指向具体 endpoint，并包含解除条件。`task blocked` 只是 projection，不是足够的原因说明。

需要人工授权时，graph 应出现 decision endpoint：

```text
human.approved(plan_revision, scope) -> A.execute
```

如果验证绑定的输入 revision 已变化，旧 `verify passed` 信号必须失效。Task Manager 根据变化影响把 task 送回 verify 或 plan；不得让 Merge Queue 在后台静默复用旧结论。

## 8. Task boundary

当内部工作满足至少一项时，Manager 才应建立新 task：

- 有独立、可测的完成条件；
- 可以独立失败或重试；
- 需要等待外部输入或人类决定；
- 需要不同权限、工作区或 owner；
- 结果被其他 task 直接依赖；
- 生命周期超过当前 phase invocation。

文件读取、一次 tool call、局部摘要或同一计划下的连续命令，应留在 Execution Graph。Task 数量衡量的是独立责任，不是运行步骤数量。

## 9. 不变量

1. Task 和 Coordination Graph 的寿命独立于 agent session。
2. Task Manager Agent 是 Coordination Graph 的唯一写入口。
3. Scheduler 只决定何时运行，不改变 Task Contract 或 edge 含义。
4. worker agent 只提交 requirement、结果和证据，不直接创建 task 或 edge。
5. 跨 task 关系尽量落到具体 Phase Endpoint。
6. Execution Graph 可以递归；递归节点不会自动成为持久 task。
7. 验证失败通常创建新 attempt，不创建新 task。
8. `verify passed` 必须绑定输入 revision 和证据；相关输入变化后信号失效。
9. `done` 只在验收和交付条件全部满足后成立。
10. 杀掉所有 agent 进程不能抹掉任何未完成义务。
