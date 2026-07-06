# Workspace / Git / Merge Queue 详细设计

版本：v0.1  
状态：Draft

---

## 1. 定位

Merge Queue 负责 verify passed 结果的合并、diff 观察和并发冲突协调，让 verify 成为进入项目事实的闸门。

worktree 隔离本身不在这里单独实现。第一阶段 worktree、branch、cwd、git 能力由 Agent Runtime 包装 CLI agent 得到（见 [Agent Runtime 详细设计](./agent-runtime.md)）。本文档描述这些隔离产出如何进入 verify 和 merge。

---

## 2. 基本规则

```text
1. 每个 task attempt 在 Agent Runtime 包装出的 worktree/cwd 隔离环境中执行。
2. agent 只能在自己被分配的隔离环境中修改。
3. main branch 只能由 merge queue 修改。
4. task verify 通过后才允许进入 merge queue。
5. merge 前需要基于最新 main 重新验证。
6. merge 前需要检查 active conflicts。
7. merge 后必须写入 Event Log，并通过 Ctx Agent 写入 Context Lib。
```

---

## 3. Worktree 数据模型

```ts
Worktree {
  id: string
  task_id: string
  attempt_id: string

  path: string
  branch_name: string

  base_commit: string
  head_commit?: string

  status:
    | "created"
    | "active"
    | "verifying"
    | "merge_queued"
    | "merged"
    | "abandoned"

  created_at: string
  updated_at: string
}
```

---

## 4. Branch 命名

```text
task/{task_id}-{short_slug}/attempt-{n}
```

例如：

```text
task/T123-agent-runtime-wrapper/attempt-01
```

---

## 5. Merge Queue

Merge Queue 负责把 verify passed 的 task 串行合入 main。

### 合入流程

```text
1. task verify passed
2. 进入 merge queue
3. 检查 active conflicts
4. rebase / merge latest main
5. targeted verify
6. 生成 merge summary
7. commit
8. 创建 merge context block
9. 标记 task done
10. 通知相关 active tasks
```

### Merge 不变量

```text
1. 未通过 verify 的 task 不得 merge。
2. merge 前必须基于最新 main 重新检查。
3. merge 前必须检查 active conflict。
4. merge 后必须写入 ctxlib。
5. merge 后必须更新 task graph projection。
```

---

## 6. 并发与冲突协调

多 agent 并发时，冲突协调必须避免对称等待。

核心原则：

```text
已经 verify passed 并进入 merge queue 的 task 优先。
仍在 execute 或 planning 的 task 需要单边适配。
```

---

## 7. 冲突类型

```text
file_conflict:
  两个 task 修改同一文件。

api_conflict:
  一个 task 修改 API contract，另一个 task 基于旧 contract 工作。

semantic_conflict:
  两个 task 修改同一状态机、业务规则或所有权边界。

test_conflict:
  一个 task 改测试预期，另一个 task 改实现。

ownership_conflict:
  task 修改了不属于自己 owner module 的状态。
```

---

## 8. Write Set

每个 task 有两个 write set。

### Declared Write Set

plan 阶段声明：

```text
- 预计修改的模块
- 预计修改的文件
- 预计修改的 API
- 预计修改的数据库表
- 预计修改的测试
```

### Observed Write Set

execute 后从 diff 中提取：

```text
- 实际修改文件
- 实际修改 symbol
- 实际修改 contract
- 实际修改测试
```

---

## 9. Conflict Context Broadcast

当 Task A verify 通过并准备 merge，发现 Task B 仍活跃且有重叠，系统给 Task B 发送 conflict context。

```ts
ConflictContext {
  source_task_id: string
  target_task_id: string

  source_status: "verify_passed" | "merge_queued" | "merged"

  changed_files: string[]
  changed_modules: string[]
  changed_contracts: string[]

  diff_summary: string
  decision_summary: string

  required_adaptation:
    | "review_only"
    | "rebase_required"
    | "replan_required"
    | "split_task_required"
    | "stop_due_to_superseded"

  evidence_refs: string[]
}
```

---

## 10. 接收方处理规则

```text
如果 conflict 影响 approved plan：
  target task -> replan_required

如果 conflict 只影响实现细节：
  target task -> continue execute with adaptation

如果 conflict 使当前任务无效：
  target task -> blocked / superseded / split child task

如果 conflict 需要人类决策：
  target task -> waiting_human
```

---

## 11. Git 不变量

```text
1. 每个 task attempt 独立 worktree。
2. execute 不直接修改 main。
3. merge 前必须 verify。
4. merge 前必须检查 active conflict。
5. merged task 的 context 对未完成 task 有优先级。
6. main branch 只接受 merge queue 的写入。
```
