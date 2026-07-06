# Context Lib 详细设计

版本：v0.1  
状态：Draft

---

## 1. 定位

Context Lib 是项目级上下文库，用于取代传统 session memory。

它不是聊天记录仓库，而是结构化记忆系统。它保存经过提取、标注和验证的项目事实、设计判断、失败经验、冲突信息和用户偏好。

核心原则：

```text
不保存 session，保存结构化项目记忆。
不全量注入，按 task、phase、scope、confidence、freshness 选择。
不禁止运行时检索，但检索必须结构化、可审计、受预算限制。
```

所有对 ctxlib 的读写都必须经过 Ctx Agent，见下一节。

---

## 1.1 Ctx Agent

Ctx Agent 是 ctxlib 的唯一受控访问入口。其他 agent 不直接读取 ctxlib 底层存储，而是向 Ctx Agent 发请求。

### 为什么需要一个专门的 agent

ctxlib 是项目长期记忆，如果每个 agent 都能自由读写，会出现：

```text
- 上下文污染：把 superseded 或低置信内容当事实注入。
- 越权读取：读到与当前 task 无关或敏感的上下文。
- 预算失控：一次塞入过多 context block。
- 无法审计：不知道谁在什么时候读了什么、写了什么。
```

因此 Ctx Agent 统一负责筛选、摘要、权限、预算和事件记录。

### 职责

```text
1. 在新 agent 启动前，为其 task/phase 选择并渲染 context pack。
2. 处理运行中其他 agent 的 ctxlib 查询（CtxQuery）。
3. 按 scope、validity、visibility、risk、budget 过滤 context block。
4. 决定注入哪一层摘要（one_line / short / long / body_ref）。
5. 写入新的 context block，并做去重、supersede、标注。
6. 记录所有读写为事件，便于追溯。
7. 发现上下文矛盾时，返回 replan/human_decision 建议。
```

### 访问模型

```ts
CtxAccessRequest {
  requester: { agent_id: string; task_id: string; phase: string }

  op:
    | "build_context_pack"  // 启动前构建 pack
    | "query"               // 运行时查询（见 CtxQuery）
    | "write_block"         // 写入新 context block
    | "supersede_block"     // 标记旧 block 被替代

  payload: unknown          // 对应 op 的具体参数
}
```

### 边界

```text
- 只有 Ctx Agent 能直接访问 ctxlib 底层存储。
- 运行中的 agent 只能通过 Ctx Agent 查询，不能扩大自身权限。
- Ctx Agent 不把未经验证的猜测提升为高置信事实。
- 每次访问都必须落事件日志。
```

运行时查询协议（CtxQuery / CtxQueryResult）见第 9 节。

---

## 2. 存储内容

ctxlib 存储：

```text
- 架构决策
- 模块摘要
- 任务摘要
- 验收结果
- 失败原因
- 冲突分析
- 用户偏好
- API contract 约束
- 风险说明
- rejected approaches
- implementation notes
- test evidence
```

不直接存储：

```text
- 未过滤的完整 session
- 原始 chain-of-thought
- 无结构长日志
- 与项目无关的聊天内容
```

原始日志可以进入 artifact store，但 ctxlib 中只保留摘要、证据引用和结构化元数据。

---

## 3. Context Block 数据模型

```ts
ContextBlock {
  id: string

  title: string
  summary: string
  body_ref: string

  type:
    | "architecture_decision"
    | "design_rationale"
    | "module_summary"
    | "task_summary"
    | "verification_result"
    | "failure_analysis"
    | "conflict_note"
    | "user_preference"
    | "api_contract"
    | "domain_rule"
    | "risk_note"
    | "implementation_note"
    | "test_evidence"
    | "rejected_approach"

  source:
    | "human"
    | "agent_summary"
    | "tool_log"
    | "verification"
    | "merge"
    | "scheduler"
    | "imported_doc"

  confidence: number
  importance: number
  freshness: number

  validity:
    | "active"
    | "stale"
    | "superseded"
    | "disputed"
    | "experimental"
    | "deprecated"

  repo_scope: string
  module_scope: string[]
  file_scope: string[]
  symbol_scope: string[]

  task_scope: string[]
  phase_scope: ("plan" | "execute" | "verify" | "merge")[]

  semantic_tags: string[]

  supersedes: string[]
  superseded_by?: string

  visibility:
    | "all_agents"
    | "planner_only"
    | "executor_only"
    | "verifier_only"
    | "human_only"
    | "same_task_only"
    | "same_module_only"

  risk:
    | "normal"
    | "sensitive"
    | "destructive"
    | "credentials_related"

  evidence_refs: string[]

  created_at: string
  updated_at: string

  author_agent_id?: string
  originating_task_id?: string
}
```

---

## 4. Context Block 表示层级

每个 context block 应该有多层摘要：

```text
1. one_line_summary
2. short_summary
3. long_summary
4. body_ref
5. raw_artifact_ref
```

调度器根据 token budget 选择合适粒度。

例如：

```text
one_line_summary:
Codex wrapper 不能假设 CLI 一定支持稳定 JSON output。

short_summary:
Codex CLI 在 headless 模式下可能只能稳定输出 text，因此统一 runtime 需要外层 parser、schema retry 和 fallback。

long_summary:
包含失败场景、命令、替代方案、对 Agent Runtime 的设计影响。

raw_artifact_ref:
原始运行日志。
```

---

## 5. Context Curator

Context Curator 是 ctxlib 的记忆提取和维护逻辑，作为 Ctx Agent 的一部分或其下游组件运行。它负责“从事件里提炼出值得长期保存的记忆”，而 Ctx Agent 负责“对外的受控读写入口”。

它可以由规则引擎、embedding 检索和 LLM agent 共同实现。

### 职责

1. 从 agent 输出中提取有价值的 context。
2. 从 tool call summary 中提取操作日志。
3. 从 verify failure 中提取失败经验。
4. 从 merge result 中提取最终项目事实。
5. 为 context block 打标签。
6. 计算 confidence、freshness、importance。
7. 识别 superseded context。
8. 为新 task phase 生成 context pack。
9. 发现上下文矛盾时触发 replan。
10. 维护 active task conflict map。

### Context 提取原则

进入 ctxlib 的内容必须满足至少一个条件：

```text
1. 会影响未来架构选择。
2. 会影响模块边界。
3. 会影响验收策略。
4. 解释了一次失败。
5. 记录了一个已验证事实。
6. 说明了一个被拒绝方案及原因。
7. 捕获了用户长期偏好。
8. 描述了活跃冲突。
```

不应进入 ctxlib 的内容：

```text
1. 临时闲聊。
2. 单次无意义工具输出。
3. 没有证据的猜测。
4. 已被后续事实完全覆盖的旧结论。
5. 大段未压缩日志。
```

---

## 6. Agent 启动时应拥有哪些记忆？

每个 agent invocation 应该拥有五层上下文。

```text
L0: System / role rules
L1: Current task contract
L2: Phase-specific brief
L3: Selected context pack
L4: Retrieval protocol
```

### L0：System / Role Rules

包括：

```text
- 当前 agent 角色
- 可用工具
- 禁止事项
- 输出 schema
- task 三阶段规则
- worktree 规则
- 高风险操作规则
- 子 task 创建规则
```

### L1：Current Task Contract

必须包含：

```text
- task id
- task title
- task description
- parent task
- current phase
- delivery_type
- acceptance criteria
- blockers
- dependencies
- worktree id
- base commit
- owner module
- declared write set
```

### L2：Phase-Specific Brief

不同 phase 的上下文不同。

#### Plan agent 需要：

```text
- 用户目标
- 父 task 目标
- 架构边界
- 相关模块摘要
- 已知风险
- rejected approaches
- 子 task 创建规则
- 验收标准要求
```

#### Execute agent 需要：

```text
- approved plan
- 文件范围
- 实现约束
- API contract
- 相关 implementation notes
- 不允许私自扩大 scope
- 遇到计划错误时请求 replan
```

#### Verify agent 需要：

```text
- acceptance criteria
- diff summary
- observed write set
- failure history
- risk notes
- active conflict context
- test strategy
```

### L3：Selected Context Pack

从 ctxlib 选择出的上下文包。

它应包含：

```text
- 必须遵守的架构决策
- 相关模块摘要
- 相邻任务结果
- 历史失败
- 风险说明
- 冲突上下文
- 可选但未注入的 context 索引
```

### L4：Retrieval Protocol

agent 不需要一开始看到所有 ctxlib，但必须知道如何在必要时查询。

---

## 7. Context Block 筛选维度

用于筛选 context 的维度至少包括：

### A. 类型

```text
architecture_decision
module_summary
task_summary
verification_result
failure_analysis
conflict_note
user_preference
api_contract
risk_note
implementation_note
test_evidence
rejected_approach
```

### B. 范围

```text
repo_scope
module_scope
file_scope
symbol_scope
api_scope
database_scope
ui_scope
test_scope
```

### C. 生命周期

```text
validity
freshness
created_at
updated_at
supersedes
superseded_by
```

### D. 置信度

```text
confidence
importance
evidence_refs
verified_by
```

### E. 任务关系

```text
task_scope
parent_task_id
child_task_ids
phase_scope
related_failures
blocked_tasks
```

### F. 冲突关系

```text
touched_files
declared_write_set
observed_write_set
ownership_claims
active_task_refs
conflict_risk
```

### G. 可见性和风险

```text
visibility
risk
permission_required
```

### H. token 成本

```text
summary_token_cost
long_body_token_cost
raw_artifact_size
```

---

## 8. Context Pack 生成流程

Context Pack 生成分为四步：

```text
1. hard filter
2. candidate retrieval
3. reranking
4. context pack assembly
```

### Step 1：Hard Filter

先排除不可用 context。

排除条件：

```text
- validity = superseded
- visibility 不允许当前 agent
- risk 超出当前权限
- scope 与当前 task 完全不匹配
- freshness 太低且无 evidence
```

### Step 2：Candidate Retrieval

多路召回：

```text
- task graph 邻近召回
- module/file/symbol scope 召回
- semantic embedding 召回
- recent failure 召回
- architecture decision 召回
- user preference 召回
- active conflict 召回
```

不要只依赖 embedding。

### Step 3：Reranking

打分公式：

```text
score =
  task_relevance
+ phase_relevance
+ module_overlap
+ file_overlap
+ dependency_overlap
+ freshness
+ confidence
+ importance
+ failure_risk
+ human_priority
- staleness_penalty
- token_cost_penalty
- contradiction_penalty
```

### Step 4：Context Pack Assembly

```ts
ContextPack {
  id: string
  task_id: string
  phase: "plan" | "execute" | "verify"

  included_blocks: ContextBlockRef[]

  rendered_sections: {
    must_follow: string
    relevant_decisions: string
    module_context: string
    recent_task_history: string
    known_failures: string
    conflict_context: string
    retrieval_instructions: string
  }

  omitted_but_available: ContextBlockRef[]

  token_budget_used: number
  generated_at: string
}
```

---

## 9. 运行时 ctxlib 检索

task 开始后允许检索 ctxlib，但必须受控。

### 允许检索的场景

```text
1. 当前上下文不足以理解模块。
2. 发现代码事实和 context 冲突。
3. verify 失败，需要查历史失败。
4. 检测到 active conflict。
5. 需要创建 child tasks。
6. 架构决策不明确。
7. rebase 后发现相关文件被别人修改。
```

### 不允许检索的场景

```text
1. 用 ctxlib 替代阅读当前代码。
2. 为了扩大 task scope。
3. execute 阶段擅自推翻 approved plan。
4. 查询与当前 task 无关的长期记忆。
5. 重复查询同类信息但没有新证据。
```

### CtxQuery

运行时检索必须结构化。

```ts
CtxQuery {
  task_id: string
  phase: "plan" | "execute" | "verify"

  intent:
    | "find_architecture_decisions"
    | "find_module_context"
    | "find_failure_history"
    | "find_conflict_context"
    | "find_related_tasks"
    | "find_user_preferences"
    | "find_api_contract_notes"
    | "find_verification_evidence"

  scope: {
    modules?: string[]
    files?: string[]
    symbols?: string[]
    task_ids?: string[]
  }

  max_blocks: number
  max_tokens: number
}
```

### CtxQueryResult

```ts
CtxQueryResult {
  blocks: ContextBlockPreview[]

  recommended_action:
    | "continue"
    | "append_context"
    | "restart_phase"
    | "replan_required"
    | "human_decision_required"

  contradictions: Contradiction[]
  token_estimate: number
}
```

### Replan 触发器

以下情况应触发重新 plan：

```text
1. execute 发现 approved plan 依赖的事实是错的。
2. verify 失败且不是局部修复可解决。
3. ctxlib 检索到高置信架构约束与当前实现冲突。
4. active conflict 影响当前 task 的 write set。
5. child task 结果改变父 task 方案。
6. 预算不足，需要缩小目标。
```

---

## 10. Context 不变量

```text
1. agent 启动不加载全量 ctxlib。
2. context block 必须有 type、scope、validity、confidence。
3. superseded context 默认不可注入。
4. raw logs 默认不直接进 prompt。
5. 运行时 ctxlib 检索必须记录事件。
6. 高影响 context 必须有 evidence。
```
