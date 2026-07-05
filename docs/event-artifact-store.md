# Event Log / Artifact Store 详细设计

版本：v0.1  
状态：Draft

---

## 1. 定位

Event Log 是系统事实来源。

task 表、ctxlib 索引、UI 状态、进度面板都可以视为 event log 的 projection。

Artifact Store 保存大对象，例如 transcript、tool output、test output、diff patch 和 screenshots。

---

## 2. 设计原则

```text
1. 先记录事件，再更新 projection。
2. 大对象进 Artifact Store，Event Log 只保存引用。
3. ctxlib 从 Event Log 和 Artifact Store 中提取结构化记忆。
4. merge、verify、conflict、human decision 都必须可追溯。
5. 系统状态应尽可能能从 Event Log 重放。
```

---

## 3. 核心事件

```text
TaskCreated
TaskPrepared
TaskPlanned
TaskBlocked
TaskUnblocked
TaskExecuting
TaskVerifying
TaskVerifyPassed
TaskVerifyFailed
TaskConflictDetected
TaskMergeQueued
TaskMerged
TaskDone

AgentStarted
AgentFinished
AgentFailed
AgentCreatedChildTasks

ContextBlockCreated
ContextBlockSuperseded
ContextPackSelected
ContextQueryExecuted

WorktreeCreated
WorktreeDiffObserved
WorktreeAbandoned

ConflictContextBroadcasted
HumanDecisionRequested
HumanDecisionRecorded
```

---

## 4. Event 数据模型

```ts
Event {
  id: string
  type: string

  task_id?: string
  agent_invocation_id?: string
  worktree_id?: string
  context_block_id?: string

  payload: unknown

  artifact_refs: string[]
  created_at: string
}
```

---

## 5. Artifact Store 保存内容

Artifact Store 保存较大的对象：

```text
- 原始日志
- agent transcript
- tool output
- test output
- diff patch
- screenshots
- benchmark result
- generated reports
```

ctxlib 只引用 artifact，不直接内嵌大文件。

---

## 6. Artifact 数据模型

```ts
Artifact {
  id: string
  type:
    | "agent_transcript"
    | "tool_output"
    | "test_output"
    | "diff_patch"
    | "screenshot"
    | "benchmark_result"
    | "generated_report"

  path_or_blob_ref: string
  content_hash: string

  task_id?: string
  agent_invocation_id?: string
  created_at: string
}
```

---

## 7. Projection

Event Log 可以生成多个 projection：

```text
TaskProjection:
  当前 task 状态、phase、依赖和 blocker。

AgentProjection:
  当前 active agents、历史 invocation 和结果。

CtxProjection:
  当前可用 context blocks、supersede 关系和索引。

MergeProjection:
  当前 merge queue、已合入 task 和冲突关系。

UIPanelProjection:
  用户界面展示的进度、预算和风险。
```

---

## 8. 与 ctxlib 的关系

ctxlib 不直接从 agent session 读取长期记忆，而是从事件和 artifact 中提取结构化 context。

```text
AgentFinished / TaskVerifyFailed / TaskMerged
  -> Context Curator 读取 summary 和 artifact
  -> 生成 ContextBlock
  -> ContextBlockCreated 写回 Event Log
```

---

## 9. 不变量

```text
1. 关键状态变化必须写 Event Log。
2. 大对象必须写 Artifact Store，并用 ref 引用。
3. Verify failure 必须可追溯到测试输出或人工判断。
4. Merge 必须可追溯到 verify result、diff 和 commit。
5. Human decision 必须显式记录。
6. ctxlib 中的高影响 context 必须有 event 或 artifact evidence。
```
