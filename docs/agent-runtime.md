# Agent Runtime 核心设计

版本：v0.2
状态：Draft

---

## 1. 定位

Agent Runtime 负责把 Claude Code、Codex、Gemini CLI 和其他 headless CLI agent 包装成统一 worker，供 Control Plane 调度。

它的目标不是抹平所有 agent 的能力差异，而是提供一条稳定边界：

```text
Control Plane / Scheduler
  -> Agent Runtime
  -> Agent Adapter
  -> Claude Code / Codex / Gemini / Custom CLI
```

第一阶段只实现 Claude Code 的最小完整包装：

```text
- 检测 Claude Code CLI 和认证状态。
- 声明能力。
- 在指定 worktree / cwd 中 headless 启动。
- 注入 system prompt、task prompt 和 context pack。
- 控制允许工具和权限模式。
- 解析 stream-json 输出为统一 AgentEvent。
- 记录 Event Log 和 Artifact Store 引用。
- 汇总 AgentResult 返回给 Control Plane；Task Manager Agent、Verifier 和 Merge Queue 分别按权责消费。
```

---

## 2. 设计判断

参考 Open Design 的 agent adapter 架构，Agent Runtime 采用两层包装：

```text
1. Agent Adapter Layer
   面向不同 CLI agent，处理 detect / capabilities / run / cancel / resume / stream parse。

2. Runtime Orchestration Layer
   面向本系统，处理 task attempt、worktree、context pack、event log、artifact、result 汇总。
```

Open Design 对应关系：

```text
Open Design AgentAdapter.detect       -> 本系统 AgentAdapter.detect
Open Design AgentAdapter.capabilities -> 本系统 AgentCapabilities
Open Design AgentAdapter.run          -> 本系统 AgentAdapter.run(params)
Open Design AgentEvent                -> 本系统统一 AgentEvent，并额外保留 raw provider event
Open Design artifact workspace        -> 本系统 task attempt worktree / artifact refs
Open Design product run               -> 本系统 Control Plane / Scheduler 管理的 task phase attempt
```

本系统不会照搬 Open Design 的设计 artifact / preview / plugin marketplace，而是借鉴 adapter 边界、能力协商和事件归一化方式。

关键判断：

```text
1. adapter 是 CLI 归一化边界。
   Scheduler 不知道 claude/codex/gemini 的具体 flags。

2. capability 只表达调度和产品真正需要的能力。
   底层 flags 留在 adapter 内部。

3. event 要小而稳定。
   provider 原始事件保存在 raw 中，向上只暴露统一 AgentEvent。

4. workspace / artifact 是一等对象。
   Runtime 不只保存 stdout，还要观察 diff、文件写入、transcript 和测试证据。

5. fallback 不能静默发生。
   从一个 adapter 切到另一个 adapter 必须显式记录，并受策略控制。
```

---

## 3. 非目标

第一阶段不追求：

```text
1. 同时完整支持所有 CLI agent。
2. 暴露每个 CLI 的所有命令行参数给上层。
3. 自己重新实现一个独立于 CLI 的工具系统。
4. 自己重新实现一个独立于 CLI 的 worktree 抽象。
5. 让 executor 在没有 plan / requirement 的情况下扩大 scope。
6. 让 verifier 自我批准自己或同一 active context 的执行结果。
7. 让 agent 直接写 Task Graph、ctxlib 或 main branch。
```

---

## 4. 总体结构

```text
┌──────────────────────────────────────────────┐
│              Control Plane / Scheduler       │
│  task phase / role / budget / capacity       │
└───────────────────────┬──────────────────────┘
                        │
                        ▼
┌──────────────────────────────────────────────┐
│                 Agent Runtime                │
│  prepare / invoke / observe / summarize      │
└───────────┬──────────────────────┬───────────┘
            │                      │
            ▼                      ▼
┌──────────────────────┐  ┌────────────────────┐
│    Agent Adapter     │  │ Event / Artifact   │
│ detect/run/parse     │  │ log refs / blobs   │
└───────────┬──────────┘  └────────────────────┘
            │
            ▼
┌──────────────────────────────────────────────┐
│       CLI Agent in isolated cwd/worktree     │
│ Claude Code / Codex / Gemini / Custom        │
└───────────────────────┬──────────────────────┘
                        │
                        ▼
┌──────────────────────────────────────────────┐
│      AgentResult + observed write set        │
│  summary / status / artifacts / event refs   │
└──────────────────────────────────────────────┘
```

一句话概括：

```text
Agent Runtime 从 Task Graph 接收一个 task phase attempt，准备 context 和 workspace，选择合适 adapter 运行 CLI agent，把 CLI 输出解析成统一事件，观察实际写入和 diff，最后返回可验收的 AgentResult。
```

---

## 5. 核心接口

## 5.1 AgentAdapter

`AgentAdapter` 是不同 CLI agent 的唯一归一化边界。

```ts
AgentAdapter {
  id: string
  display_name: string
  provider: "claude" | "codex" | "gemini" | "custom"

  detect(): Promise<AgentDetection | null>
  capabilities(): AgentCapabilities

  run(params: AgentRunParams): AsyncIterable<AgentEvent>
  cancel(run_id: string): Promise<void>

  resume?(
    run_id: string,
    message: string
  ): AsyncIterable<AgentEvent>
}
```

adapter 负责：

```text
- 找到 CLI executable。
- 判断版本和认证状态。
- 构造 provider-specific spawn 命令。
- 决定 prompt 通过 argv、stdin、JSON-RPC 还是 HTTP 传递。
- 设置 cwd、env、权限、工具和可读目录。
- 解析 stdout / stderr / JSONL / JSON-RPC / SSE。
- 把 provider 原始事件映射成统一 AgentEvent。
```

---

## 5.2 AgentDetection

CLI 存在不代表可以无头运行，因此 detection 需要记录认证和配置状态。

```ts
AgentDetection {
  provider: "claude" | "codex" | "gemini" | "custom"

  executable_path: string
  version: string

  config_dir?: string
  native_skills_dir?: string

  auth_state: "ok" | "missing" | "expired" | "unknown"

  install_hint?: string
  error?: string
}
```

检测结果用于：

```text
- 判断 adapter 是否可调度。
- 在 UI 中提示缺少 CLI、缺少认证或版本不支持。
- 决定是否允许 fallback 到其他 adapter。
```

---

## 5.3 AgentCapabilities

Capability 不描述所有 CLI flags，只描述调度和上层产品需要知道的能力。

```ts
AgentCapabilities {
  supports_headless: boolean
  supports_streaming: boolean
  supports_structured_output: boolean
  supports_tool_calling: boolean
  supports_file_edit: boolean
  supports_shell: boolean
  supports_mcp: boolean

  // worktree/cwd/git 隔离优先使用 CLI 自身能力；不支持时由 wrapper 兜底。
  supports_git_worktree: boolean
  supports_additional_directories: boolean

  supports_resume: boolean
  supports_native_skill_loading: boolean
  supports_surgical_edit: boolean

  permission_mode: "strict" | "permissive" | "none"

  context_window_hint?: number
  cost_model?: CostModel

  default_roles: AgentRole[]
}
```

调度使用 capability 做硬约束：

```text
- 需要改文件的 execute task 不能调度到不支持 file_edit 的 adapter。
- 需要运行测试的 verifier 不能调度到不支持 shell 的 adapter。
- 需要实时 UI 的运行优先选择 supports_streaming 的 adapter。
- skill 要求 native skill loading 时，不支持的 adapter 必须改用 prompt injection 或被排除。
```

---

## 5.4 AgentRunParams

`AgentRunParams` 是 adapter 的稳定输入。它表达运行意图，不暴露 provider-specific flags。

```ts
AgentRunParams {
  run_id: string
  invocation_id: string

  cwd: string
  worktree_id?: string

  role: AgentRole
  phase: "plan" | "execute" | "verify" | "conflict"

  system_prompt: string
  user_prompt: string

  context_pack_dir?: string
  skill_dir?: string

  allowed_tools?: ToolCapability[]
  timeout_ms?: number
  budget_limit?: BudgetLimit

  output_schema?: JsonSchema

  metadata: {
    task_id: string
    attempt_id: string
    requirement_refs: string[]
  }
}
```

provider-specific 设置留在 adapter 内部或 adapter config 中，例如 Claude Code adapter 可以内部选择：

```text
claude -p --output-format stream-json --verbose --permission-mode <mode>
```

而 Codex / Gemini adapter 可以选择自己的 headless 命令、stdin 策略和 stream parser。

---

## 5.5 AgentEvent

`AgentEvent` 是 Runtime 向 Event Log、UI 和 projection 暴露的统一流式事件。

```ts
AgentEvent =
  | AgentThinkingEvent
  | AgentTextDeltaEvent
  | AgentToolCallEvent
  | AgentToolResultEvent
  | AgentFileWriteEvent
  | AgentErrorEvent
  | AgentDoneEvent
```

### AgentThinkingEvent

```ts
AgentThinkingEvent {
  type: "thinking"
  run_id: string
  text: string
  raw?: unknown
}
```

### AgentTextDeltaEvent

```ts
AgentTextDeltaEvent {
  type: "text_delta"
  run_id: string
  text: string
  raw?: unknown
}
```

### AgentToolCallEvent

```ts
AgentToolCallEvent {
  type: "tool_call"
  run_id: string
  tool_call_id?: string
  name: string
  input?: unknown
  raw?: unknown
}
```

### AgentToolResultEvent

```ts
AgentToolResultEvent {
  type: "tool_result"
  run_id: string
  tool_call_id?: string
  output?: unknown
  is_error?: boolean
  raw?: unknown
}
```

### AgentFileWriteEvent

如果 provider 没有原生 file write event，Runtime 可以通过 write-set 观察合成该事件。

```ts
AgentFileWriteEvent {
  type: "file_write"
  run_id: string
  path: string
  operation?: "create" | "modify" | "delete"
  raw?: unknown
}
```

### AgentErrorEvent

```ts
AgentErrorEvent {
  type: "error"
  run_id: string
  message: string
  raw?: unknown
}
```

### AgentDoneEvent

```ts
AgentDoneEvent {
  type: "done"
  run_id: string
  reason: "completed" | "cancelled" | "error" | "timeout"
  raw?: unknown
}
```

原则：

```text
1. 上层只依赖统一 AgentEvent。
2. provider 原始事件必须保留 raw，便于审计和后续 parser 修正。
3. 大输出不直接塞进 event，进入 Artifact Store，用 ref 关联。
4. Event Log 由 Runtime 自动记录，agent 不显式写日志。
```

---

## 5.6 AgentResult

`AgentResult` 是一次 invocation 的最终汇总。

```ts
AgentResult {
  invocation_id: string
  run_id: string
  task_id: string
  attempt_id: string
  phase: "plan" | "execute" | "verify" | "conflict"

  status:
    | "succeeded"
    | "failed"
    | "needs_replan"
    | "expanded_task_graph"
    | "blocked"
    | "conflict_detected"
    | "cancelled"
    | "timeout"

  summary: string
  structured_output?: unknown

  // agent 声明的修改范围和 Runtime 观察到的真实修改范围。
  touched_files_declared: string[]
  touched_files_observed: string[]

  // agent 不能直接创建 task / edge，只能提交 requirement 请求。
  submitted_requirement_refs: string[]

  context_queries: string[]
  artifact_refs: string[]
  event_refs: string[]

  usage?: {
    duration_ms?: number
    token_usage?: unknown
    cost_usd?: number
  }
}
```

`expanded_task_graph` 的含义不是 agent 直接写了 task graph，而是它提交了 requirement，Task Manager Agent 已经或将要把它编排成 task graph 变更。

---

## 6. Runtime 执行流程

一次 task phase attempt 的执行流程：

```text
1. Scheduler 选择可运行 task phase 和角色。
2. Runtime 根据角色和 capability 选择 adapter。
3. Runtime 准备 cwd/worktree、context pack、task contract 和 output schema。
4. Runtime 生成 system prompt 与 user prompt。
5. Runtime 调用 adapter.run(params)。
6. adapter 启动 CLI agent 并解析输出为 AgentEvent。
7. Runtime 自动写入 Event Log，并把大对象写入 Artifact Store。
8. Runtime 观察 worktree diff 和 touched files。
9. Runtime 汇总 AgentResult。
10. Control Plane 路由 AgentResult：Task Manager Agent 负责 Task Graph 写入，Verifier 负责验收，Merge Queue 负责合并。
```

流程示意：

```text
Task Contract
  -> Context Pack
  -> Workspace Binding
  -> AgentRunParams
  -> AgentAdapter.run()
  -> AgentEvent stream
  -> Event Log / Artifact Store
  -> Observed Write Set
  -> AgentResult
```

---

## 7. Workspace / Worktree / Git

第一阶段不单独实现一套与 agent 无关的 worktree 系统。worktree、git、cwd、可写范围和 tool 权限先作为 CLI wrapper 能力包装。

基本原则：

```text
1. 每个 task attempt 在独立 cwd/worktree 中执行。
2. 优先复用 CLI agent 自身的 worktree / cwd / git 能力。
3. 如果 CLI 不支持 worktree，则由 Runtime 用 git worktree 或独立 clone 兜底。
4. agent 不能直接修改 main branch。
5. Runtime 观察实际 diff，并将结果交给 Verify / Merge Queue。
6. Merge Queue 才能把 verify passed 的结果合入 main。
```

Workspace 绑定：

```ts
WorkspaceBinding {
  worktree_id: string
  cwd: string
  base_ref: string
  branch_name?: string
  writable_roots: string[]
  readable_roots: string[]
}
```

观察结果：

```ts
ObservedWriteSet {
  worktree_id: string
  changed_files: string[]
  created_files: string[]
  deleted_files: string[]
  diff_artifact_ref?: string
}
```

---

## 8. Prompt / Context / Skill 注入

Runtime 不依赖 agent session memory。每次 invocation 都显式注入必要上下文。

上下文层次：

```text
1. Runtime policy / role boundary
2. Task contract
3. Context pack from Ctx Agent
4. Approved plan 或 acceptance criteria
5. Skill / workflow instruction（可选）
6. User prompt / phase-specific instruction
7. Output schema
```

Skill 注入支持多种模式：

```ts
SkillInjectionMode =
  | "native"       // 安装或 symlink 到 agent 原生 skill 目录。
  | "prompt"       // 将 SKILL.md / references inline 到 prompt。
  | "project_file" // 写入 .cursorrules 等 agent-specific 文件。
  | "unsupported"
```

选择规则：

```text
- adapter 支持 nativeSkillLoading 时，优先 native。
- 不支持 native 时，使用 prompt injection。
- 需要 agent-specific 规则文件时，使用 project_file。
- skill 要求的 capability 不满足时，不能调度或必须降级为受限模式。
```

---

## 9. Tool / Permission Policy

Runtime 用统一策略表达权限意图，adapter 负责翻译成具体 CLI flags。

```ts
ToolCapability {
  name: string
  matcher?: string
  mode: "allow" | "deny"
}
```

```ts
PermissionPolicy {
  mode:
    | "default"
    | "plan"
    | "accept_edits"
    | "auto"
    | "dont_ask"
    | "bypass"

  require_human_approval_for_high_risk: boolean
}
```

原则：

```text
1. planner 默认不能改代码。
2. executor 只能在 approved scope 内改代码。
3. verifier 默认不修改实现，只运行检查和报告结果。
4. 高风险操作必须显式人工批准。
5. `dont_ask` / `bypass` 只允许在明确授权的本地调试或受控执行中使用，并且不能覆盖高风险人工批准要求。
6. agent-generated edits 默认作为 pending changes / worktree diff，不直接进入 main。
```

---

## 10. Claude Code MVP Adapter

第一阶段 Claude Code adapter 是 reference adapter。

必须支持：

```text
- detect: claude 是否存在、版本、auth 状态。
- capabilities: headless、streaming、tool calling、file edit、shell、MCP、worktree。
- run: 使用 print/headless 模式启动。
- parse: 将 stream-json / JSONL 转成统一 AgentEvent。
- cancel: 终止进程。
- result: 汇总 final result、usage、cost、session id 和 touched files。
```

建议内部命令形态：

```text
claude -p
  --output-format stream-json
  --verbose
  --permission-mode <mode>
  --allowedTools / --disallowedTools ...
  --max-turns <n>
```

Claude Code 原始事件映射：

```text
system/init           -> Event Log 的 AgentStarted / metadata
assistant text        -> text_delta
assistant tool_use    -> tool_call
user tool_result      -> tool_result
result                -> done + AgentResult usage/status
hook events           -> raw event 或后续扩展事件
partial messages      -> text_delta 或 raw event
```

MVP 不要求完整支持 Claude Code 的全部 flags，但必须保留 raw 事件，避免后续无法补 schema。

---

## 11. Fallback 策略

Fallback 用于 adapter 不可用、认证失效、运行失败或 capability 不满足时。

```ts
FallbackPolicy {
  allow_fallback: boolean
  require_explicit_switch: boolean
  candidates: string[]
}
```

原则：

```text
1. 不静默切换 provider。
2. fallback 必须记录到 Event Log。
3. 如果任务依赖某个 provider 的特定能力，不能 fallback 到不支持该能力的 provider。
4. fallback 后的 result 必须标注实际执行 adapter。
```

---

## 12. 与其他模块关系

### Task Graph

```text
Task Graph 提供 task contract、phase、role、acceptance criteria 和状态。
Agent Runtime 返回 AgentResult 和 event refs。
Control Plane 只能路由结果；Task Graph 的状态、edge、blocker 写入仍由 Task Manager Agent 负责。
```

### Ctx Agent / Context Lib

```text
Ctx Agent 为 invocation 选择 context pack。
Agent Runtime 只消费 context pack，不直接读写 ctxlib。
agent 运行中需要更多上下文时，通过受控 ctx query 进入 Ctx Agent。
```

### Event Log / Artifact Store

```text
Runtime 自动记录 AgentStarted、AgentEvent、AgentFinished、AgentFailed、WorktreeDiffObserved 等事件。
transcript、stdout/stderr、大 tool output、diff patch、test output 进入 Artifact Store。
```

### Verify / Merge Queue

```text
Verifier 消费 AgentResult、ObservedWriteSet、test output 和 acceptance criteria。
Merge Queue 只合并 verify passed 的 worktree diff；Agent Runtime 不执行 main branch merge。
```

---

## 13. 不变量

```text
1. Agent Runtime 是 CLI agent 的唯一启动入口。
2. Scheduler / Task Graph 不依赖 provider-specific flags。
3. agent 不直接写 Task Graph；只能提交 requirement。
4. agent 不直接写 ctxlib；ctxlib 只从 Event Log / Artifact Store 提炼。
5. agent 不直接修改 main branch。
6. 每次 invocation 必须有 workspace/cwd 边界、role 边界和 tool/permission 边界。
7. 每次 invocation 必须自动进入 Event Log。
8. 大对象必须进入 Artifact Store，Event Log 只保存 ref。
9. observed write set 以 Runtime 观察为准，agent 声明只能作为参考。
10. verifier 不能自我批准同一 active context 的执行结果。
11. fallback 不能静默发生。
12. 第一阶段以 Claude Code adapter 跑通最小闭环，再扩展 Codex / Gemini / custom。
```

---

## 14. MVP 范围

MVP 需要完成：

```text
1. AgentAdapter 接口。
2. Claude Code adapter detect / capabilities / run / cancel。
3. AgentRunParams、AgentEvent、AgentResult 数据结构。
4. stream-json 到 AgentEvent 的 parser。
5. workspace/cwd 绑定和 observed write set。
6. Event Log / Artifact Store 写入 refs。
7. role-based tool / permission policy。
8. plan / execute / verify 三类 role 的 prompt boundary。
9. AgentResult 作为 Task Manager Agent 推进 Task Graph 状态的输入证据。
```

MVP 可以暂缓：

```text
1. 完整 Codex / Gemini adapter。
2. 复杂 resume。
3. 多 provider 自动 fallback。
4. 完整 MCP server 编排。
5. UI 级实时细节展示。
6. 高级 skill/plugin marketplace。
```
