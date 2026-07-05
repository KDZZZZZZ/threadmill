# Agent Runtime 详细设计

版本：v0.1  
状态：Draft

---

## 1. 定位

Agent Runtime 负责把 Claude Code、Codex、Gemini CLI 和其他 headless CLI agent 包装成统一 worker。

它的目标不是抹平所有 agent 的能力差异，而是用统一协议描述能力、启动任务、收集结果和记录事件。

---

## 2. 支持对象

包括但不限于：

```text
- Claude Code
- Codex CLI
- Gemini CLI
- 自定义 headless agent
```

---

## 3. Capability Profile

不同 CLI agent 能力不同，因此系统使用 capability profile 描述每个 agent 能做什么。

```ts
AgentCapabilityProfile {
  id: string
  display_name: string

  provider: "claude" | "codex" | "gemini" | "custom"
  cli_path: string
  version: string

  supports_headless: boolean
  supports_structured_output: boolean
  supports_tool_calling: boolean
  supports_mcp: boolean
  supports_file_edit: boolean
  supports_shell: boolean
  supports_git_worktree: boolean

  context_window?: number
  cost_model?: CostModel

  default_roles: AgentRole[]
}
```

---

## 4. Agent Role

同一个 CLI agent 可以被调度成不同角色：

```text
- planner
- executor
- verifier
- reviewer
- conflict_resolver
- context_curator
```

角色决定 prompt contract、工具权限、输出 schema 和可接受的行为边界。

---

## 5. Agent Invocation

```ts
AgentInvocation {
  id: string
  task_id: string
  attempt_id: string

  phase: "plan" | "execute" | "verify" | "conflict" | "merge"
  role: "planner" | "executor" | "verifier" | "reviewer"

  agent_runtime_id: string
  model?: string

  worktree_id: string
  context_pack_id: string

  prompt_contract: string
  output_schema: JsonSchema

  allowed_tools: ToolCapability[]
  budget_limit: BudgetLimit
  timeout_ms: number
}
```

---

## 6. Agent Result

```ts
AgentResult {
  invocation_id: string
  task_id: string
  phase: string

  status:
    | "succeeded"
    | "failed"
    | "needs_replan"
    | "created_child_tasks"
    | "blocked"
    | "conflict_detected"

  summary: string
  structured_output: unknown

  created_tasks: string[]
  touched_files_declared: string[]
  touched_files_observed: string[]

  context_queries: string[]
  artifact_refs: string[]
  event_refs: string[]
}
```

---

## 7. Wrapper 职责

每个 CLI wrapper 需要负责：

```text
1. 检测 CLI 是否存在。
2. 获取 CLI 版本和能力。
3. 支持 headless 启动。
4. 注入 prompt 和 context pack。
5. 限制工具权限和工作目录。
6. 捕获 stdout/stderr/transcript。
7. 解析 structured output。
8. 对不支持 structured output 的 agent 做外层 parser 和 retry。
9. 将过程写入 Event Log。
10. 将大输出写入 Artifact Store。
```

---

## 8. 角色边界

### Planner

```text
- 可以拆 child tasks。
- 可以定义 acceptance criteria。
- 可以声明 write set。
- 不应该直接修改代码。
```

### Executor

```text
- 只能按照 approved plan 实现。
- 不应私自扩大 scope。
- 发现计划错误时请求 replan。
- 输出 observed write set 和 self-check。
```

### Verifier

```text
- 根据 acceptance criteria 验收。
- 默认不修改实现。
- 输出 pass/fail、证据和 failure reason。
- 不能自我批准自己执行的结果。
```

### Context Curator

```text
- 从事件和结果中提取可复用 context。
- 标注 type、scope、confidence、freshness。
- 不应把未经验证的猜测提升为高置信事实。
```

---

## 9. Agent 不变量

```text
1. agent 不拥有长期记忆，ctxlib 拥有长期记忆。
2. agent 输出必须结构化摘要。
3. agent 不能私自扩大 task scope。
4. execute agent 发现计划错误时应请求 replan。
5. verify agent 不能自我批准 execute 结果。
6. agent 只能在分配的 worktree 和权限范围内执行。
```
