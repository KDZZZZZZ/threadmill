# Scheduler / Budget 详细设计

版本：v0.1  
状态：Draft

---

## 1. 定位

Scheduler 负责根据 task graph、agent capacity、预算、风险和依赖关系决定下一步启动什么。

Budget Model 负责约束系统可投入的 token、时间、并发、retry 和 verify 强度。

---

## 2. Scheduler 输入

```text
- task graph
- worker pool capacity
- agent capability profiles
- budget status
- task priority
- blocked status
- active conflicts
- merge queue
```

---

## 3. Scheduler 输出

```text
- start planner
- start executor
- start verifier
- create context pack
- create worktree
- pause task
- replan task
- merge task
```

---

## 4. 调度原则

```text
1. blocked task 不调度。
2. dependencies 未完成的 task 不调度。
3. 高优先级 task 优先。
4. verify 阶段优先于新 execute，因为 verify 可以释放 merge，并解除依赖它的 blocked task。
5. merge queue 优先处理已验证结果。
6. 有冲突风险的 task 降低并发。
7. budget 不足时减少探索性 agent。
8. capability 不匹配的 agent 不接任务。
```

---

## 5. 用户操作语义

### agent +1

用户点击：

```text
agent +1
```

系统行为：

```text
增加 worker capacity。
Scheduler 自动分配下一个合适 task phase。
```

这不是给新 agent 手动分配任务，而是增加系统吞吐。

### 提交新需求

用户输入：

```text
“需求：支持 Codex wrapper。”
```

系统行为：

```text
登记 requirement。
由 Task Manager 编排 task / state node / edge。
根据依赖、状态 endpoint 和预算排入 task graph。
不一定立刻启动 agent。
```

---

## 6. Budget Model

预算不只是金钱，也包括：

```text
- token
- 时间
- 并发数
- shell 执行成本
- retry 次数
- verify 强度
```

### BudgetPolicy

```ts
BudgetPolicy {
  max_tokens?: number
  max_cost_usd?: number
  max_wall_time_ms?: number
  max_agent_invocations?: number
  max_retries?: number

  verify_level:
    | "basic"
    | "standard"
    | "strict"
    | "paranoid"

  exploration_level:
    | "low"
    | "medium"
    | "high"
}
```

用户表达：

```text
“我需要这个功能，投入 5 个 agent，最多跑 30 分钟。”
```

系统转化为：

```text
worker_capacity = 5
wall_time_budget = 30min
scheduler_policy = maximize_verified_tasks
```

---

## 7. Replan 触发器

Scheduler 应在以下情况触发重新 plan：

```text
1. execute 发现 approved plan 依赖的事实是错的。
2. verify 失败且不是局部修复可解决。
3. ctxlib 检索到高置信架构约束与当前实现冲突。
4. active conflict 影响当前 task 的 write set。
5. 相关 task 结果改变当前 task 方案。
6. 预算不足，需要缩小目标。
```

---

## 8. 优先级策略

建议默认优先级：

```text
1. merge queue 中已通过 verify 的 task。
2. 被 blocked task 或 phase endpoint 依赖的 task。
3. verify 阶段 task，尤其是解锁其他 task.verify 的依赖。
4. 已 planned 且无冲突风险的 execute task。
5. 来自新 requirement 的 task planning。
6. 探索性或低优先级任务。
```

---

## 9. Scheduler 不变量

```text
1. 不调度 blocked task。
2. 不把 task 分配给 capability 不匹配的 agent。
3. 不让 execute 跳过 plan。
4. 不让 merge 跳过 verify。
5. agent capacity 只影响吞吐，不改变 task graph 语义或 Task Manager 的依赖编排权。
6. budget 不足时优先保护 verify 和 merge，而不是继续开新探索。
```
