# Task Graph 详细设计

版本：v0.2
状态：Draft

---

## 1. 定位

Task Graph 管理所有 task 节点、task 之间的图关系、阻塞、依赖、冲突和验收状态。

用户提出的不是 root task，而是 **requirement**：原始需求、目标、约束、预算和验收意图。Task Manager Agent 将 requirement 规整成一个或多个统一的 task 节点，并把需求原话作为 provenance 保留。

Task Graph 不区分 root task / child task。所有工作单元都是同一种 `Task`，区别只来自图关系：

```text
requirement = 用户或 agent 提出的需求 / 意图 / 约束，作为 provenance。
task        = 可计划、可执行、可验收的工作单元。
state node  = task 生命周期中的可寻址状态 / phase 节点，例如 A.verify、B.done。
edge        = task 或 state node 之间的依赖、阻塞、拆解、重叠、冲突、替代等关系。
```

系统不使用 session 作为工作单元，而使用 task 作为工作单元。每个 task 都可以独立经历 plan、execute、verify，并可以被阻塞、重试、拆分或合并。为了表达更细粒度依赖，task 的状态变换也被视为 task 内部的一个子图；依赖关系可以指向整个 task，也可以指向某个 task 的特定状态 / phase，例如 `Task A.verify depends_on Task B.done`。

所有对 task graph 的写入都必须经过 Task Manager Agent，见下一节。

---

## 1.1 Task Manager Agent（摘要）

Task Manager Agent 是 task graph 的**唯一写入口**。人类和 planner / executor / verifier 都不直接创建或修改 task，而是向它提交 requirement；它在写入前拥有全局 task 视图，做去重、依赖推断、阻塞判断和验收校验。

核心要点：

```text
- 只把需求转成任务契约(what/why/done)，不产出 how(how 属于 plan 阶段)。
- 人类需求用宽松模式(可规整/合并/提炼验收)；agent requirement 用严格契约模式
  (带 client_ref、内容不可被改写、重复只 link 不 merge)。
- planner / executor / verifier 在拆解时提交 requirement，不直接创建 task / edge。
- 内容字段归发起方，依赖编排、图关系与元数据归 Task Manager 且只可新增。
- 权威写动作只有写 Task Graph；其活动被 runtime 自动记入 Event Log，不写 ctxlib。
```

完整设计（intake 模式、内容/图关系分权、client_ref 幂等、验收归属、决策集合、
与其他模块的交互、不变量）见：[Task Manager Agent 详细设计](./task-manager-agent.md)。

---

## 2. Requirement、Task、Edge

### Requirement

Requirement 是需求记录，不是可调度工作单元。

```text
Requirement:
  "做一个统一 Claude Code、Codex、Gemini CLI 的多 agent vibe coding 控制台。"
```

requirement 的作用是保留原始意图、约束、预算和验收意图。它可以被一个 task 满足，也可以被多个 task 共同满足；也可能因为重复、不可行或等待澄清而暂时不生成 task。

### Task

Task 是统一的工作节点。

```text
Tasks:
  - 实现 CLI agent discovery
  - 实现 headless wrapper
  - 实现 task graph 状态机
  - 实现 ctxlib 检索策略
  - 实现 conflict-aware merge queue
```

这些 task 没有 root / child 类型差异。一个 task 如果需要被拆解，Task Manager 只是在图里新增 task 节点和 typed edges。

### Edge

Edge 表达 task 或 task state node 之间的关系。

```text
Task A --depends_on--> Task B
Task B --blocks-----> Task A
Task A --decomposes_to--> Task C
Task D --overlaps_with--> Task E
Task F --conflicts_with--> Task G
Task A.verify --depends_on--> Task B.done
Task A.execute --depends_on--> Task C.planned
```

因此，task graph 的核心不是层级树，而是：

```text
task graph = requirements + task nodes + task state subgraphs + typed edges
```

---

## 3. 核心设计

一个 task 不一定必须直接交付代码。对于复杂 task，允许它的合法交付是提出新的 requirement，由 Task Manager Agent 扩展 task graph：新增更小的 task 节点，并建立 task / state node 之间的依赖、阻塞和拆解关系。

这意味着：

```text
执行 task 的 planner / executor / verifier 可以提交 requirement：
- 为什么当前 task 需要额外工作
- 需要什么新工作单元或决策
- 这个 requirement 的验收意图
- 触发该 requirement 的 source task + phase / status
- 本地 client_ref，便于幂等回显
```

planner / executor / verifier 不直接创建 task 或 edge，也不直接决定跨 task 依赖。Task Manager Agent 拥有全局 task 视图，负责把这些 requirement 编排成 task 节点、state node 依赖和 blockers。

当一个 task 被拆解后，它不进入特殊层级状态，而是进入 `blocked` 或阻塞某个 state node，并通过 `TaskEdge` / `Blocker` 说明它被哪些 task、哪些 task state 或哪些决策阻塞。相关 endpoint 满足后，Scheduler 可将它重新推进到 planning、executing 或 verifying。

---

## 4. 数据模型

### TaskGraph

```ts
TaskGraph {
  requirements: Requirement[]
  tasks: Task[]
  state_nodes: TaskStateNode[] // 可物化，也可由 task 状态机投影生成
  edges: TaskEdge[]
}
```

### Requirement

```ts
Requirement {
  id: string
  requester:
    | "human"
    | {
        agent_id: string
        role: "planner" | "executor" | "verifier" | "reviewer" | "conflict_resolver"
        task_id: string
        source_phase?: "plan" | "execute" | "verify" | "conflict" | "merge"
        source_status?: string
      }

  raw_text: string
  goal?: string
  constraints: string[]
  acceptance_intent?: string[]
  budget_hint?: BudgetHint
  priority_hint?: "low" | "medium" | "high"

  // 发起 agent 的本地幂等键；human requirement 可为空。
  client_ref?: string

  created_at: string
}
```

### Task

```ts
Task {
  // 任务唯一 ID。Task 不编码 root / child 层级。
  id: string

  // 用户、planner 或其他 agent 可读的任务标题与描述。
  title: string
  description: string

  // 指向原始需求或 agent 请求，保留 provenance。
  requirement_refs: string[]

  status:
    | "created"          // 已创建，但尚未准备运行。
    | "prepared"         // 已完成 worktree、context pack、task contract 等准备。
    | "planning"         // planner 正在制定方案。
    | "planned"          // 计划已生成，等待 execute。
    | "executing"        // executor 正在实现。
    | "verifying"        // verifier 正在验收。
    | "blocked"          // 被依赖、其他 task、冲突或人类决策阻塞。
    | "conflict"         // 检测到与其他 task 或 main branch 的冲突。
    | "merging"          // 已通过 verify，正在等待或执行 merge。
    | "done"             // 已通过验收并完成交付。
    | "failed"           // 无法继续或明确失败。
    | "waiting_human"    // 需要人类做产品、风险或权限决策。

  phase:
    | "prepare"  // 创建 worktree、选择上下文、生成 task contract。
    | "plan"     // 规划方案，也可以向 Task Manager 提出新的 requirement。
    | "execute"  // 按 approved plan 执行。
    | "verify"   // 根据验收标准验证。
    | "conflict" // 处理冲突上下文和 replan。
    | "merge"    // 进入 merge queue 并合入。

  delivery_type:
    | "code_change"         // 交付代码改动。
    | "graph_expansion"     // 交付 task graph 扩展：提交 requirement 并由 Task Manager 新增 task / state edge / blocker。
    | "research_report"     // 交付调研结论。
    | "design_decision"     // 交付架构或产品设计决策。
    | "test_plan"           // 交付测试计划。
    | "verification_result" // 交付验收结果。
    | "conflict_analysis"   // 交付冲突分析和协调建议。

  // 任务完成的验收标准。task 只有通过验收后才算结束。
  acceptance_criteria: AcceptanceCriterion[]

  // 当前阻塞原因。跨 task 依赖由 TaskEdge 表达，blocker 记录调度可读原因。
  blockers: Blocker[]

  // 当前 task 声明负责的模块，用于边界控制和冲突检测。
  owner_module?: string

  // plan 阶段声明预计修改哪些文件；execute 后记录实际修改哪些文件。
  touched_files_declared: string[]
  touched_files_observed: string[]

  // 每个 task attempt 应在独立 worktree 中执行。
  worktree_id?: string
  base_commit: string
  current_commit?: string

  // 调度用字段：优先级、风险、预算和上下文策略。
  priority: number
  risk_level: "low" | "medium" | "high"

  budget_policy: BudgetPolicy
  context_policy: ContextPolicy

  created_at: string
  updated_at: string
}
```

### TaskStateNode

```ts
TaskStateNode {
  id: string
  task_id: string

  // task 生命周期中的可寻址节点。
  phase?: "prepare" | "plan" | "execute" | "verify" | "conflict" | "merge"
  status?: "created" | "prepared" | "planning" | "planned" | "executing" | "verifying" | "blocked" | "conflict" | "merging" | "done" | "failed" | "waiting_human"

  // 用于表达 A.verify 依赖 B.done 这类细粒度关系。
  address: string // 例如 "task:A:verify" 或 "task:B:done"
}
```

### TaskEdge

```ts
GraphEndpoint =
  | { type: "task"; task_id: string }
  | { type: "task_state"; task_id: string; phase?: string; status?: string }

TaskEdge {
  id: string

  // from -> to 的语义由 type 决定；endpoint 可以是整个 task，也可以是 task state node。
  from: GraphEndpoint
  to: GraphEndpoint

  type:
    | "depends_on"       // from endpoint 依赖 to endpoint 满足。
    | "blocks"           // from endpoint 阻塞 to endpoint。
    | "decomposes_to"    // from 被拆解出 to；不表示层级类型，只表示来源关系。
    | "overlaps_with"    // 两个 task / endpoint 范围重叠。
    | "duplicates"       // 两个 task / endpoint 等价或重复。
    | "supersedes"       // from 取代 to。
    | "conflicts_with"   // 两个 task / endpoint 存在写集或语义冲突。

  reason: string

  // 依赖编排的权威来源永远是 Task Manager；agent requirement 只作为证据和触发来源。
  source: "task_manager"
  requested_by?: "human" | "planner" | "executor" | "verifier" | "merge_queue"
  requirement_refs: string[]
  evidence_refs: string[]

  created_at: string
}
```

---

## 5. Task 状态机

```text
created
  ↓
prepared
  ↓
planning
  ↓
planned
  ↓
executing
  ↓
verifying
  ├── passed
  │     ↓
  │   merging
  │     ↓
  │   done
  │
  ├── failed
  │     ↓
  │   planning
  │
  ├── conflict
  │     ↓
  │   conflict_resolution
  │     ↓
  │   planning / executing / verifying
  │
  └── blocked
        ↓
      planning / executing / verifying
```

这条状态机也可以投影成一个 task 内部子图：

```text
Task A.prepare -> Task A.plan -> Task A.execute -> Task A.verify -> Task A.done
```

跨 task 依赖可以挂在任意 endpoint 上，而不是只能挂在整个 task 上：

```text
Task A.verify depends_on Task B.done
Task A.execute depends_on Task C.planned
Task D.plan    depends_on Task E.verify.passed
```

Scheduler 读取的是这些 endpoint 是否满足，而不是只看“父子 task 是否全部 done”。

---

## 6. 状态含义

### created

任务已创建，但尚未准备运行。

### prepared

已经完成基础准备，例如 worktree 创建、task contract 生成、初始 context pack 选择。

### planning

planner agent 正在制定方案。

### planned

计划已生成，等待执行。

### executing

executor agent 正在实现。

### verifying

verifier agent 正在验收。

### blocked

任务无法继续，需要等待其他 task、外部依赖、冲突处理或人类决策。阻塞关系必须通过 `Blocker` 和 / 或 `TaskEdge` 表达。

### conflict

检测到与其他 task 或 main branch 的冲突。

### merging

任务通过 verify，正在进入 merge queue 或执行 merge。

### done

任务已验收并合入，或其 delivery_type 已满足验收条件。

---

## 7. Task 交付规则

task 可以通过以下方式完成：

```text
1. 提交代码改动并通过 verify。
2. 提交新的 requirement，由 Task Manager 扩展 task graph：创建新的 task / state edge / blocker，并且当前 task 的验收标准允许这种交付。
3. 产出研究报告或设计决策，并通过 review。
4. 产出 test plan 或 verification result。
5. 产出 conflict analysis 并触发后续协调动作。
```

---

## 8. 图扩展规则

一个 task 通过 planner / executor / verifier 提交 requirement 后：

```text
当前 phase agent -> 向 Task Manager 提交 requirement（严格模式，带 client_ref）
Task Manager -> 编排 task / state node / edge / blocker
当前 task 或当前 state node -> blocked（如果需要等待新增 task、特定 state endpoint 或人类决策）
新增 task -> 独立 plan-execute-verify
被依赖 endpoint 满足后 -> 当前 task 重新进入 planning / executing / verifying
```

当前 task 不应因为所有相关 task done 就自动 done。它仍需确认自身验收标准是否整体达成；如果依赖只挂在 `A.verify`，则 `A.execute` 仍可先行，直到 verify 前等待 `B.done`。

---

## 9. Task 不变量

```text
1. task 未通过 verify 不得 merge。
2. requirement 不是 task；用户原话必须作为 provenance 保留。
3. task 不区分 root / child 类型；拆解、依赖和阻塞都由 typed edges 表达。
4. planner / executor / verifier 拆解任务时提交 requirement，不直接创建 task / edge。
5. 依赖关系由 Task Manager Agent 基于全局视图编排。
6. task 状态变换可视为子图；依赖可以指向 task，也可以指向 task 的 phase / status endpoint。
7. task 可通过提交 requirement 并触发 task graph 扩展作为合法交付。
8. blocked 不是 failed。
9. 被依赖 endpoint 满足后，等待方 task 必须重新检查自身验收或当前 phase gate。
10. execute 前必须存在 plan 和 acceptance criteria。
11. verify 失败必须产生 failure context，供下一轮 plan 使用。
12. task graph 只能由 Task Manager Agent 权威写入。
```
