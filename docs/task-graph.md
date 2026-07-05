# Task Graph 详细设计

版本：v0.1  
状态：Draft

---

## 1. 定位

Task Graph 管理所有任务、子任务、依赖、阻塞和验收状态。

系统不使用 session 作为工作单元，而使用 task 作为工作单元。每个 task 都可以独立经历 plan、execute、verify，并可以被阻塞、重试、拆分或合并。

---

## 2. Root Task 与 Child Task

**Root task** 是用户直接提出的顶层目标任务。它代表“用户最终想要达成的结果”。

```text
Root task:
  做一个统一 Claude Code、Codex、Gemini CLI 的多 agent vibe coding 控制台。
```

root task 通常不会直接由一个 agent 一次性完成。它更像是 task graph 的根节点，用来承载总体目标、预算、优先级和最终验收标准。

**Child task** 是系统、planner 或 agent 为了完成 root task 而拆出来的子任务。

```text
Child tasks:
  - 实现 CLI agent discovery
  - 实现 headless wrapper
  - 实现 task graph 状态机
  - 实现 ctxlib 检索策略
  - 实现 conflict-aware merge queue
```

root task 和 child task 的关系是：

```text
root task = 用户目标的顶层容器
child task = 为完成 root task 而拆出的可执行工作单元

task graph = root task + child tasks + task 之间的依赖、阻塞和验收关系
```

因此，创建 root task 不等于立即启动一个 agent；它只是把用户目标放进系统。Scheduler 会根据预算、依赖、优先级和 worker capacity 决定什么时候启动 planner、executor 或 verifier。

---

## 3. 核心设计

一个 task 不一定必须直接交付代码。对于复杂任务，允许 task 的合法交付是创建一组更小的 child tasks。

这意味着：

```text
大 task 可以只负责：
- 架构拆解
- 子任务定义
- 验收标准
- 模块边界
- 风险识别
```

父 task 创建 child tasks 后进入 blocked/waiting_children 状态，等待子任务完成。

---

## 4. Task 数据模型

```ts
Task {
  // 任务唯一 ID。root task 和 child task 都是 Task，只是层级不同。
  id: string

  // 用户或 planner 可读的任务标题与描述。
  title: string
  description: string

  // parent_task_id 为空表示这是 root task，也就是用户直接提出的顶层目标。
  // parent_task_id 有值表示这是 child task，也就是从某个父任务拆出来的子任务。
  parent_task_id?: string

  // 当前任务拆出的子任务列表。复杂任务可以通过创建 child tasks 作为合法交付。
  child_task_ids: string[]

  status:
    | "created"          // 已创建，但尚未准备运行。
    | "prepared"         // 已完成 worktree、context pack、task contract 等准备。
    | "planning"         // planner 正在制定方案。
    | "planned"          // 计划已生成，等待 execute。
    | "executing"        // executor 正在实现。
    | "verifying"        // verifier 正在验收。
    | "blocked"          // 被依赖、子任务、冲突或人类决策阻塞。
    | "waiting_children" // 父任务已拆出子任务，正在等待子任务完成。
    | "conflict"         // 检测到与其他 task 或 main branch 的冲突。
    | "merging"          // 已通过 verify，正在等待或执行 merge。
    | "done"             // 已通过验收并完成交付。
    | "failed"           // 无法继续或明确失败。
    | "waiting_human"    // 需要人类做产品、风险或权限决策。

  phase:
    | "prepare"  // 创建 worktree、选择上下文、生成 task contract。
    | "plan"     // 规划方案，也可以拆 child tasks。
    | "execute"  // 按 approved plan 执行。
    | "verify"   // 根据验收标准验证。
    | "conflict" // 处理冲突上下文和 replan。
    | "merge"    // 进入 merge queue 并合入。

  delivery_type:
    | "code_change"         // 交付代码改动。
    | "task_decomposition"  // 交付一组子任务，而不是直接实现全部内容。
    | "research_report"     // 交付调研结论。
    | "design_decision"     // 交付架构或产品设计决策。
    | "test_plan"           // 交付测试计划。
    | "verification_result" // 交付验收结果。
    | "conflict_analysis"   // 交付冲突分析和协调建议。

  // 任务完成的验收标准。task 只有通过验收后才算结束。
  acceptance_criteria: AcceptanceCriterion[]

  // 显式依赖和阻塞原因。task graph 就是 task 状态机加上这些依赖关系。
  dependencies: TaskDependency[]
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
      waiting_children
        ↓
      planning / verifying
```

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

任务无法继续，需要等待子任务、外部依赖、冲突处理或人类决策。

### waiting_children

父 task 已创建 child tasks，等待子任务完成。

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
2. 创建子 task graph，并且父 task 的验收标准允许这种交付。
3. 产出研究报告或设计决策，并通过 review。
4. 产出 test plan 或 verification result。
5. 产出 conflict analysis 并触发后续协调动作。
```

---

## 8. 父子任务规则

父 task 创建 child tasks 后：

```text
父 task -> blocked / waiting_children
子 task -> 独立 plan-execute-verify
全部子 task done 后 -> 父 task 重新进入 planning 或 verifying
```

父 task 不应因为所有子 task done 就自动 done。父 task 仍需确认整体目标是否达成。

---

## 9. Task 不变量

```text
1. task 未通过 verify 不得 merge。
2. task 可通过创建 child tasks 作为合法交付。
3. blocked 不是 failed。
4. child tasks 完成后，父 task 必须重新检查整体验收。
5. execute 前必须存在 plan 和 acceptance criteria。
6. verify 失败必须产生 failure context，供下一轮 plan 使用。
```
