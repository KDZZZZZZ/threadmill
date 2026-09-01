# 模型上下文块清单与稳定性排序

本文按当前默认 `threadmill.yaml` 和运行时代码，列出每个 Agent 可能发送给模型的上下文块，并按相邻请求之间的字节稳定性排序。本文的“块”对应 `agent.Request` 的 `SystemPrompt`（C0 角色提示）、`Messages`（C4/C5）、`StateBlocks`（C3，按 ID 覆盖的状态块）、`Suffix`（C6 条件性尾部）、`Tools` 以及隐藏 compact 请求中的逻辑边界。

## 稳定性等级

等级越小越稳定：

| 等级 | 含义 | 典型内容 |
| --- | --- | --- |
| C0 | 配置常量 | 角色系统提示、固定协议文字 |
| C1 | Agent 实例级固定 | 工具集合、工具 schema、固定订阅结构 |
| C2 | Task 级契约 | Task Info、root 的原始用户请求节点 |
| C3 | 状态版本块 | 记忆图、子图、协调图快照 |
| C4 | 可追加历史 | 已有 ReAct 消息前缀、已保存的对话 |
| C5 | 当前回合/调用 | 上游交接、工具输出、当前 query、模型返回 |
| C6 | 条件性尾部 | 压力提醒、JSON 重试、取消信息 |

同一等级内，前面的结构通常比后面的正文更稳定。这里的排序是缓存稳定性排序，不代表 JSON 字段发送顺序；wire 上的实际排列见下文「发送顺序与前缀缓存」。

## 实际请求形状

正常 ReAct 请求在 [`internal/agent/input.go`](../internal/agent/input.go) 中组装：

```text
Request {
  SystemPrompt   // 语义收窄为 C0 角色提示；hook 不再往里追加
  Messages[]
  StateBlocks[]  // C3：记忆投影、协调图投影，按 ID 覆盖（SetBlock）
  Suffix         // C6：压力提醒等条件性尾部
  Tools[]
  CacheKey       // prompt_cache_key，取 agentID
}
```

每次模型调用都会重新经过 `AssembleRequest` hooks，入口是 [`internal/agent/loop.go`](../internal/agent/loop.go)。早先 Responses Provider 把同一个 SystemPrompt 放在两个位置（`input[0]` 的 system 项和顶层 `instructions` 字段），客户端侧已确认是双写——[internal/provider/responses.go](../internal/provider/responses.go) 既压入 `input[0]` 又设 `Instructions`。该双写已随缓存优化 Phase 1 移除：现在只保留 `input[0]` 的 system 项（与网关兼容性和 `Store:false` 的无状态回放同域），待验证的只剩网关侧行为。

Wire 拼装顺序（Provider 端）：

```text
input[0]           = {role:"system", content:SystemPrompt}   // C0
input[1..]         = Messages                                // C4/C5
后续 system 项      = StateBlocks 各块                        // C3
最后一个 system 项  = Suffix                                  // C6
tools              = Tools（顶层字段）
```

尾部插入 system 项不会割裂 `function_call`/`function_call_output` 配对：ReAct 循环保证每次 generate 前工具调用已配对（工具结果先于下一次 generate 追加进历史）。

正常 Responses 请求只会发送这些 Message 字段：

- user：`Content`
- assistant：`Content`、工具调用的 ID/名称/arguments
- assistant 有 `ModelData` 时：原样回放 `ModelData`，并跳过普通文本和工具调用
- tool：`ToolResult.CallID` 和 `ToolResult.Content`

正常请求不会发送 `Thinking`、`Usage`、时间戳、停止原因、错误元数据或 `ToolResult.Details`。compact 请求的历史序列化会额外包含 `Thinking`，但仍不会把这些运行元数据作为独立字段发送。

当前默认角色、工具和 hook 配置在 [`threadmill.yaml`](../threadmill.yaml)。`prompts.default` 只有在角色没有专用 `system_prompt` 时才回退，不与专用 prompt 拼接；配置仍为空时使用 [`DefaultSystemPrompt`](../internal/agent/loop.go)。压力提醒也有 [`drop_context.go`](../internal/agent/drop_context.go) fallback，深度整理的动态 user 文本由 [`deepCurationQuery`](../internal/agent/curation.go) 生成。逐项来源见 [`agent-prompts-after.md`](agent-prompts-after.md)。

## 发送顺序与前缀缓存

### 前缀缓存语义

OpenAI Responses 的前缀缓存是**从请求第 0 个 token 起的精确前缀匹配**：两个请求必须共享逐字节相同的前缀才可能命中；有 1024 token 的最小可缓存长度；缓存约 5–10 分钟空闲后过期。由此有两个推论：

- organizer 的短整理请求和 compact 请求（前缀 = 一段固定的整理提示词）往往凑不满 1024 token，**可能永远命中不了缓存**——除非它们与前一个大请求共享足够长的前缀。
- 任何插在历史之前的易变块都会作废其后全部缓存。

### 现状与目标顺序对照

审计时点所有 C3 块（记忆投影、协调图快照）和 C6（压力提醒）都用 `+=` 拼到 `Request.SystemPrompt` 尾部，而 SystemPrompt 是 wire 上第 0 个 token：

```text
现状:  C0 角色提示 │ C3 记忆 │ C3 协调图 │ C6 提醒 ║ C4 历史 │ C5 尾部   │ tools
目标:  C0 角色提示 │ tools ║ C4 历史 │ C3 记忆 │ C3 协调图 │ C5 │ C6
```

现状下，记忆图或协调图任何一个字节变化，都会作废其后全部 ReAct 历史的缓存。manager 最严重：`Snapshot.Revision`/`Executing` 逐请求变动，且 `coordination_orchestrate` 会在同一 ReAct 轮内改图，导致同一轮的连续工具调用互相打掉缓存。目标布局已按 Phase 2 落地：C0+tools 构成静态前缀，紧跟 append-only 的历史；C3/C6 移到历史之后。

### 排序规则的纠正

C0–C6 是**变化率分级**，不是 wire 顺序处方。「越稳定越靠前」并不完整——真正的布局规则是：

```text
静态前缀 → append-only 历史 → 易变状态 → 当轮尾部
```

关键在于：历史（C4）是 append-only 的，第 N 次请求的历史是第 N+1 次的前缀，本可无限增长为一条稳定长前缀。把易变的 C3 插在 C0 和 C4 之间恰好摧毁这条前缀；把 C3 移到历史之后，记忆或协调图的变动只作废尾部几百 token。

代价：记忆从「背景」变为「近期上下文」，模型阅读顺序改变，需要行为验证（verifier 是否仍能逐项映射 Task Info、是否仍正确引用已召回记忆）。Phase 2 验收包含该行为核对。


## 记忆节点持有态何时变化

这里的“Agent 持有记忆节点”不是一个单一集合，而是三层状态：

1. **Memory Store 图**：`env.Memory.Snapshot()` 返回的权威图，包含子图、节点和边。
2. **Loop 订阅选择器**：固定订阅与动态订阅的子图 ID；它决定哪些节点会被投影到正常请求。
3. **Messages 里的详情副本**：`memory_*` 查询、展开结果和工具结果。它们是对话历史，不等于图上的节点。

生产协调路径使用按环境隔离的 [`context.Store`](../internal/context/store.go)。代码还保留了单独的 [`context.GlobalView`](../internal/context/global.go) 兼容路径；只有显式绑定该视图的 Agent 才会读写全局图，不能把它和 task 的 Store 版本混作同一缓存域。

对配置了 `inject_subscribed_memory` 的正常模型请求，每次 `AssembleRequest` 都会重新读取当前图快照，再计算（没有该 hook 的 organizer 不走这条自动注入路径）：

```text
effective_subgraphs = unique(fixed_subgraphs + dynamic_subgraphs)
visible_nodes = graph.NodesInSubgraphs(effective_subgraphs)
memory_block = formatMemory(visible_nodes)
```

`NodesInSubgraphs` 按图中 `Nodes` 的顺序取并集并按 ID 去重；它**不会按 `status` 过滤**，所以 `superseded`/`outdated` 节点只要仍属于订阅子图，仍会带状态标签发送。`formatMemory` 只发送节点的 `kind`、`status` 和 `statement`。因此，图已经提交但请求正在生成时，不会回写正在进行的请求；它只会从下一次模型请求开始可见。工具调用或生命周期 hook 提交图后，当前工具结果仍先进入 `Messages`，下一次 `generate` 才同时看到新记忆和该结果。实现入口见 [`internal/agent/input.go`](../internal/agent/input.go) 与 [`internal/agent/hidden_tools.go`](../internal/agent/hidden_tools.go)。

Phase 3 起该投影带 memo：`Loop.subscribedMemoryBlock` 以 `(EnvView.Revision(), 有效订阅列表)` 为键复用上次的投影文本，命中时跳过隐藏工具执行。两条必须记住的边界：

- **revision 只是失效提示**。绕过显式 API、以相同 revision 提交节点变化的裸 `Store.Save` 会读到旧文本；生产写路径都会递增 revision。
- **memo 必须随 `Bind` 失效**。`Graph.Clone` 保留 `Revision`，所以 `Fork` 出的子环境与父环境 revision 相同，而订阅列表跨 `Bind` 保留；不清 memo 就会把上一个环境的记忆文本泄漏给新环境。`bindLoopTools` 换 `loop.memory` 时一并清空 memo，`TestSubscribedMemoryBlockInvalidatesOnBind` 锁定该行为。这正是本文后面「`scope` 至少要隔离 Agent/环境」的具体落点。

### 订阅选择器什么时候变

| 触发事件 | 变化的状态 | 影响范围与可见时机 |
| --- | --- | --- |
| manager 装配完成 | 固定订阅设为 `system-manager` | manager 的下一次请求开始使用该子图；checkpoint 恢复不会覆盖固定订阅。 |
| task 角色装配完成 | planner、executor、verifier 各自固定订阅 `<task-id>-package` | 三个角色的下一次请求使用 task package；三者共享 task 记忆图，但动态订阅列表各自独立。 |
| `organize_subgraph` 成功 | 只有发起调用的 requester 追加新 `sg-q-N`，并去重 | organizer 的订阅不会被改变；请求成功返回后，requester 的下一次模型请求才看到该子图节点。 |
| organizer 在查询中调用 `memory_subscribe(subscribe)` | requester 的动态订阅追加这些子图 ID | 只在 `organize_subgraph` 的整理 Ask 成功返回后统一应用；子图必须在当时的快照里存在，否则逐项跳过并在工具返回的 `subscriptions.skipped` 里说明。 |
| organizer 在查询中调用 `memory_subscribe(unsubscribe)` | requester 的动态订阅移除这些子图 ID（保序） | 同样在查询收尾统一应用，只过滤动态列表：`<task-id>-package`（stable）与 `system-manager`（fixed）在结构上不可取消，本次查询的目标 `sg-q-N` 也不可取消，三者都记入 `skipped`。移除的子图从 requester 下一次注入消失。 |
| `organize_subgraph` 失败 | requester 不追加订阅；目标空子图可能已经写入图 | 失败不会自动回滚已创建的 `sg-q-N`，所以可能留下“存在但无人订阅”的空子图；organizer 在这次查询里记录的订阅增删也**全部不生效**。 |
| `SetSubscribedSubgraphs` | **替换**整个动态订阅列表，不是追加 | 被移除的子图从下一次注入消失；固定订阅仍保留。 |
| checkpoint 恢复 | 恢复保存的 `Messages`、工具调用 ID 和动态订阅 | 图不从 checkpoint 恢复；恢复后的下一次请求会用当下 Store 的图配合旧订阅重新投影，因此可能和保存 checkpoint 时不同。 |
| `Bind` 到另一 `env.Env` | 只更换 Loop/工具使用的 MemoryView | 订阅 ID 和历史保留不变；新环境没有对应节点归属时，投影为空。 |
| fork / merge | 图快照和订阅选择器分开处理 | `Fork` 只复制图，不复制另一个 Loop 的订阅；`Merge` 只合并图，不把 child 的动态订阅传播给 parent。继承是 **fork 时刻的全量快照**：新 task 拿到 from task 当时的整张图，之后 parent 新增的节点不会自动流入 child，回流只经 join 的 additive merge。 |

取消动态订阅有两条路：`SetSubscribedSubgraphs` 整表替换，或 organizer 在一次 organize 查询里用 `memory_subscribe` 逐个取消（内部走 `Loop.unsubscribeSubgraph`）。两条路都只作用于动态列表，固定订阅与 stable 订阅不受影响。未知子图 ID 可以留在列表里；在正常无悬空归属的图中，`NodesInSubgraphs` 会忽略它。

订阅列表本身是投影 memo 的一半键，所以增删订阅会立刻让上一次投影文本失效，不需要额外的失效动作。

### 图上的节点、归属和状态什么时候变

下表的“下一请求”均指目标 Agent 下一次调用模型；已经发出的请求不会被异步刷新。

| 事件 | 具体写入 | 哪些节点/字段会变 | 可见性与特殊规则 |
| --- | --- | --- | --- |
| manager 收到普通用户消息 | `Manager` 的 `BeforeTurn` 调用 `ProjectManagerUserMessage` | 新增 `CreatorAgentID=user` 的 directive 节点 | 发生在该 turn 的第一条模型调用前，所以本条用户消息会同时出现在 manager `Messages` 和 `system-manager` 记忆中。 |
| manager 收到内部消息 | 任务报告、恢复消息或 `[拆分请求]` 通过内部 enqueue 进入 `Messages` | 这次 enqueue 本身只新增消息；任务报告节点由单独的 report projection 写入；`[拆分请求]` 明确不投影为记忆节点 | 内部消息仍会进入 manager 的模型上下文，但不会因为这条消息自动增加 `system-manager` 节点。 |
| task sink 首次注册或协调图改变 | `SetTaskSink`、`ReplacePending`、`coordination_orchestrate(action=provide_help)` 触发 `ProjectManagerTaskInfos` | 对非空 Info upsert `task-info-<id>`；必要时新增 `task-user-input-<id>` 到 `system-task-sources` | `task-info-*` 进入 manager 固定子图；`system-task-sources` 默认不在 manager 固定订阅中，主要供 root 装配时复制到 package。相同 ID 的 Task Info 是替换，不是无界追加；已开始/已完成 task 的 info 变更会被协调图拒绝。 |
| root task 装配 | `Assemble` 确保 `<task-id>-package`，复制 root 原始用户节点并追加 Task Info | 新增或更新 package 归属 | planner、executor、verifier 的固定 package 投影从该时刻起变化；helper 不重复复制 root 用户节点，只追加自己的 Task Info。 |
| task 环境初始化 | **首个** root 从 manager 环境 fork 后删除 `system-manager`；装配时还会删除继承的 `system-task-sources` | 删除对应子图及其节点、边；多重归属节点也会被删除 | 这是 root 从 manager 记忆转成最小 package 的切点。父环境以后新增的 manager 节点不会自动流入已 fork 的 task；后续 root 虽从前一 root 环境 fork，但新角色固定只订阅自己的 package，旧 package 节点默认仍留在图中而不被注入。 |
| `ProjectManagerTaskReport` / candidate report | `AppendNode` 以 `task-report-*` ID 写入 manager `system-manager` | 新报告新增；同 ID 报告可替换 statement、status、来源和创建者 | manager 下一次请求看到报告节点；报告状态按 verdict/命令证据为 `accepted` 或 `disputed`。join 的候选报告先投影到 manager，再由目标角色通过 `join` 读取文件产物。 |
| `organize_subgraph` 创建目标 | 先用 `WithSubgraph` 提交 `sg-q-N` 元数据 | 新增子图元数据，尚无节点也可能使图 revision 增加 | organizer 随后在同一环境选择节点；成功后 requester 才追加订阅。目标子图即使选中 0 个节点也会留下并被 requester 订阅。 |
| organizer 选择或创建记忆 | organizer 的 `memory_add_to_subgraph` / `memory_apply` 提交 | 既有节点可被 attach；也可 create 新节点、更新 statement/kind/status、设置 `superseded_by` 或删除 | 写入发生在 organizer 的 `Ask` 内；成功返回前已经提交。organizer 的内部对话不会复制到 requester 的 `Messages`，只有图上实际提交的变化和工具返回 JSON 会影响 requester。 |
| `memory_add_to_subgraph` | `WithNodesInSubgraph` 改写节点 `SubgraphIDs`；有至少一个有效节点时，未知目标可同时创建子图元数据 | 不创建节点正文；可让已有节点进入某个已订阅子图 | 已有 system 子图被拒绝；package 目标没有同样的拒绝，因而应把 package 归属视为会改变固定启动包的写入。 |
| `memory_apply` | `WithNodeChanges` 原子应用一批操作 | `create` 新节点；`update` 替换 statement/可选 kind/status；`status` 改 status/superseded；`delete` 删除节点及关联边；`attach/detach` 改归属 | 整批成功或整批不变。`task-info-*` 与 user/system 来源节点不可改；报告节点只能改 status；directive 不能改写或删除（只能改 status）。 |
| overflow compact | assistant 返回的 `Usage.TotalTokens >= max(1, 3/4*context_window)`，且角色配置了 `compact_on_overflow` | compact 模型把旧 Messages **追加**成新节点和边；旧节点不会被自动 update/delete | compact 节点的 `subgraph_ids` 只从非 `system`/非 `package` 子图中选择，也可以留空；订阅关系只用于生成 `derives_from_subgraph` 边，不等于正式归属。所以 compact 成功不保证新节点出现在 manager 的 `system-manager` 或 task package 固定投影中。下一次 generate 才看到它们。 |
| turn-end compact | 角色配置了 `commit_tail_on_turn_end` 且本轮正常完成 | 把剩余历史（`keep=0`）追加为节点，并把 Messages 换成尾部 | 当前默认 manager、verifier 有该 hook；planner、executor 只有 overflow hook；organizer 没有自动 compact。manager/verifier 一轮可能先 overflow compact、后 turn-end compact。 |
| compact 后深度整理 | compact 后总节点数达到 `deep_audit_max_nodes`（默认 64）或单次新增达到 `deep_audit_min_added`（默认 32），且 organizer 空闲 | organizer 可用 `memory_apply` 更新、标记、合并归属或删除节点 | 这是尽力而为；整理失败不回滚已经提交的 compact，整理过程中已成功的批次仍可能保留。 |
| child join 回 parent | `joinIncoming` 等 child 完成后调用 `Store.Merge(child, parent)` | **additive-only 不变量**：合入只允许 ① 新增节点、子图和边；② 同 ID 同 statement 节点的 `SubgraphIDs` 归属并集（附着不算修改内容）。冲突 ID 重映射后作为新节点加入 | 除归属并集外，parent 已有节点的 `statement`/`kind`/`status`/`source_refs`/`creator_agent_id`/`superseded_by` 永不被 child 改写，parent 已有子图的元数据（含 `admission`/`scope`）也不被覆盖，child 的删除不传播——child 想推翻 parent 的结论只能新增节点，由整理 Agent 在 parent 侧裁决。child 的动态订阅同样不传播。merge 在目标角色下一次 Ask 前执行，所以目标只有已订阅相关子图时才看到合入节点。锁定测试见 [`internal/context/merge_test.go`](../internal/context/merge_test.go)。 |
| 普通 EnvView commit | compact、memory 工具或外部代码调用 `Memory.Commit` / `Store.Save` | 取决于提交图的字段 | `Store.Save` 会保留当前 `system`/`package` 子图及其 runtime-managed 节点，避免普通旧快照把它们静默删除或改写；`AppendNode`、`EnsureSubgraph`、`DropSubgraph`、`Merge` 是运行时的显式管理路径。 |

底层 `Graph` 变换的精确语义在 [`internal/context/graph.go`](../internal/context/graph.go)、[`internal/context/graph_changes.go`](../internal/context/graph_changes.go) 和 [`internal/context/graph_transform.go`](../internal/context/graph_transform.go)；manager/task 的运行时触发点分别在 [`internal/coordination/stores.go`](../internal/coordination/stores.go)、[`internal/coordination/assemble.go`](../internal/coordination/assemble.go) 和 [`internal/coordination/run.go`](../internal/coordination/run.go)。

### 提交竞态与失败边界

- `Graph.WithMemory`、`WithNodesInSubgraph`、`WithSubgraph` 和 `WithNodeChanges` 都返回新图；在调用 `Memory.Commit`/`Store.Save` 之前不会改变 Store。相反，`AppendNode`、`DropSubgraph` 和 `Merge` 在 Store 锁内基于最新快照提交。
- `EnvView.Commit` 没有 compare-and-swap 或 revision 前置检查，是“读取整图 → 外部整理 → 整图替换”。同一环境有并发 compact、organizer 或 memory tool 时，后提交的旧快照可能覆盖先提交的**非** `system`/`package` 节点；`Store.Save` 只会合并保留 runtime-managed 子图/节点。缓存应以提交后的快照重新计算，不能把一次成功 commit 推断成纯追加。
- `AppendNode` 的空 ID 会分配 `system-N`；显式 ID 在同一目标子图内会做相同内容去重或整节点替换，在其他子图已有该 ID 时报错。`AppendNodes` 自身先在本地图完成整批操作，失败时不提交部分结果；但 assemble/task-sink 这类由多次 Store 调用组成的高层流程，前一步成功后后一步失败仍可能留下前一步的节点。
- `memory_apply` 的一批操作是原子的；`organize_subgraph` 则先提交空目标子图，再运行 organizer，所以 organizer 失败时目标子图元数据仍可能存在。
- `OpenStore` 会在进程启动时加载上次持久化的图和 fork baseline；加载本身不新增节点，但会使重启后的第一次请求与进程内旧缓存脱钩，缓存域应包含持久化 Store 的身份。

### 只改变模型上下文、不改变记忆图的事件

这些动作会改变下一次请求的 `Messages` 或详情形状，但不会新增、删除或更新图节点：

- `memory_neighbors`、`memory_subgraphs_of`、`memory_sources_of`、`memory_nodes_in` 是只读查询；结果作为 tool message 留在历史中。
- `memory_expand` 只改变 Loop 的展开级别并返回视图；`memory_collapse` 会从历史的 JSON 内容/详情中移除高于目标级别的节点详情；`memory_drop_from_context` 也只移除历史详情。三者都不删图、不改订阅。
- 普通 `appendMessage`、文件/命令工具结果、planner→executor→verifier 交接和 join 通知都只是 `Messages`；只有之后触发 compact，旧消息才可能被摘要成图节点。
- `serializeConversation` 会排除 `memory_*` 与 `organize_subgraph` 的调用和结果，所以“展开视图”不会因为 compact 自动原样回写成长期记忆。
- 压力提醒只写请求的 `Suffix` 段（输入末尾的 system 项），不改历史、不改记忆图；审计时它曾被拼进 SystemPrompt（wire 最前端）。checkpoint 只保存进行中的 Messages、工具 ID、动态订阅和 committed 标志，不保存图快照、固定订阅或 memory view 展开级别。

### 各 Agent 的实际变化面

| Agent | 固定订阅 | 直接影响其正常记忆投影的主要事件 | 不会自动看到的内容 |
| --- | --- | --- | --- |
| manager | `system-manager` | 用户投影、Task Info、candidate/final report、manager 自己的 compact/curation，以及自己成功召回的动态子图 | `system-task-sources` 仅在显式订阅时可见；task role 的动态订阅不会回传。 |
| planner | `<task-id>-package` | package 装配、同 task 其他角色/child 的共享图提交、planner 自己的 organize/compact，以及 organizer 在服务它的查询时对其动态订阅的增删 | executor/verifier 的动态订阅列表和未归属 compact 节点不会自动进入。 |
| executor | `<task-id>-package` | 与 planner 相同的共享 task 图；executor 自己的 organize/compact；join 前后的 child merge | planner 的动态订阅不会自动复制；候选文件必须经 join 工具处理，文件本身不是记忆节点。 |
| verifier | `<task-id>-package` | 共享 task 图、verifier 自己的 organize/compact、join 合入；verifier 的报告另写入 manager 图，不会反向改变 verifier 的固定投影 | manager 的 `system-manager` 节点不会因共享文件环境而进入 verifier package。 |
| subgraph organizer | 默认无固定/自动注入的记忆订阅 | 它在同一 env 上执行查询工具和 `memory_apply`（含 `describe_subgraph`）；深度整理也可能写图 | 不会自动收到整张图的 SystemPrompt；当前整理 query、子图目录和候选节点由用户消息/工具结果提供。它改的是**请求方**的订阅，自己的订阅不受 `memory_subscribe` 影响。 |

同一 task 的 planner、executor、verifier 共用一个 organizer Loop；因此 organizer 的 `Messages` 会跨这三个角色累积所有整理 query 和 memory 工具结果，直到该 task 的 team 被丢弃。每个 child task 会重新装配自己的 organizer，不会共享这段历史。

### 缓存失效应按“有效投影”而不是只看 revision

对普通记忆 SystemPrompt，**字节级**缓存真正需要失效的条件是 `formatMemory(graph.NodesInSubgraphs(...))` 的输出字节发生变化：

1. 有效投影中的节点 `kind`、`status` 或非空 `statement` 变化；
2. 有效投影的节点顺序或成员序列变化（`formatMemory` 保留图顺序）；
3. attach/detach、子图删除或订阅变化导致上述渲染序列变化。

节点 ID、子图 ID 或订阅列表本身变化，如果最后渲染出的 `kind/status/statement` 序列完全相同，并不会改变这段 SystemPrompt；但在需要保留身份、权限隔离或后续工具引用关系的**语义缓存**中，仍应把它们纳入键。

下列变化通常**不**改变普通记忆文本，但会改变 organizer/`memory_*` 请求，不能把它们误认为完全无关：`SourceRefs`、`CreatorAgentID`、`SupersededBy`、边、未订阅节点、子图 `name/summary/admission/scope/kind/revision` 以及无 statement 节点。

`Graph.Revision` 只能作为失效提示，不能作为内容等价证明：

- **可能误报**：`Merge` 即使没有有效新增也会加一；`DropSubgraph` 对不存在的子图也会加一；`WithSubgraph` 和 `WithNodeChanges` 对语义上相同的提交也可能加一；只改边或元数据也会改 revision，而普通记忆文本不变。
- **可能漏报**：`Store.Save` 信任调用方传入的 `Graph.Revision`，调用方可以在不递增 revision 的情况下提交字段变化；`Subgraph.Revision` 在普通 `WithMemory` 追加节点和 additive merge 时也不是可靠的节点内容版本。

建议把普通记忆块拆成独立缓存，并区分“字节缓存”和“节点身份缓存”：

```text
memory_text_bytes_key = (scope, hash(formatMemory(visible_nodes)))
memory_node_chunk_key = (
  scope,
  canonical_effective_subgraph_id_set,
  ordered[(node_id, kind, status, statement)]
)
```

`scope` 至少要隔离 Agent/环境，避免把一个 task 的受保护记忆跨环境复用。需要 `memory_expand`、organizer 或 compact 的缓存时，再把完整节点元数据、子图目录/版本、相关边、view level 和序列化历史加入键；不要复用上述精简投影键。

补充（已按 Phase 3 实现）：投影字节未变时应短路——不重算格式化输出，也不产生新版本。organizer 提交无关边或子图元数据时，普通记忆块的字节不变，正常请求的前缀不应被误伤；记忆注入路径在 Loop 上对上一次投影做了 memo（内容字节复用）。

## manager

运行时实例 ID 为 `manager`。角色提示留在 `SystemPrompt`；对话在 `Messages`，manager 记忆与协调图在可替换 `StateBlocks`，可选压力提醒在 `Suffix`。

### 稳定性排序

| 等级 | 块 | 实际内容 |
| --- | --- | --- |
| C0 | `M-01 角色系统提示` | `agents.manager.system_prompt`：manager 身份、用户输入路由、root/helper 拓扑、改图规则、验收门禁和收尾规则。 |
| C0/C1 | `M-02 工具 schema` | `coordination_orchestrate`、`organize_subgraph` 的 name、description 和 JSON Schema。描述可被顶层 `tools` 覆盖。 |
| C1 | `M-03 记忆块结构` | 固定订阅 `system-manager`；渲染为 `记忆：` 加 `- [kind/status] statement`。 |
| C3 | `M-04 manager 记忆节点` | `system-manager` 中的用户消息、Task Info、任务报告和候选任务报告；manager compact 生成的节点只有在归属当前订阅子图时才进入该块。 |
| C3 | `M-05 协调图静态说明` | `当前协调图（JSON：tasks[].id/info/outcome/sequence，edges[].from/to 为节点关联）：` 等固定说明文字。审计时该字符串还宣传了一个 `Snapshot` 里不存在的 `helps 为拆分请求` 字段（`internal/coordination/graph.go` 的 `Snapshot` 只有 `revision/executing/tasks/edges`），属真实的 prompt/schema 不一致，已随 Phase 3 从说明文字中删除。同一句里的 `sequence` 也不是 JSON 字段——`Task` 上没有该字段，`Sequence()` 是返回三个角色节点的方法，不参与序列化；且字段名实际以 Go 字段名大写序列化（`ID`/`Info`/`Outcome`），与说明文字的小写写法不一致。这两处尚未修，属已知的说明文字与 payload 偏差。 |
| C5 | `M-06 协调图快照` | 每轮重新取得的 `revision`、`executing`、`tasks`、`edges` JSON；当前快照结构没有 `helps` 字段，help 请求正文会在 manager 用户消息中出现。字节缓存意义上等价 C5：`Revision` 与 `Executing` 是逐请求易变字段，manager 的这段投影几乎每次请求都变。Phase 3 起注入前会剥掉这两个字段（`Snapshot.PromptProjection`），投影在内容不变时逐字节稳定，重新回到 C3 的稳定度。不对 tasks/edges 排序：`g.tasks`/`g.edges` 本身就是 append 有序切片，删除走保序原地压缩，字节已经稳定；按 ID 排序反而会破坏创建顺序（ID 是 `task-%d` 递增计数，字典序在第 10 个 task 之后就有 `task-10 < task-2`）。 |
| C4 | `M-07 外部用户消息` | `Manager.Send()` 入队的原始用户文本，作为 `RoleUser`。 |
| C5 | `M-08 内部 manager 用户消息` | `[拆分请求]` 通知、任务完成报告、恢复所需的内部消息，也都作为 `RoleUser`。 |
| C4/C5 | `M-09 manager ReAct 历史` | 之前的 user、assistant 文本、assistant 工具调用、工具结果和 Responses `ModelData`。 |
| C5 | `M-10 协调工具结果` | `coordination_orchestrate` 两种 action 返回的完整图快照；`organize_subgraph` 返回的 `{subgraph, subscriptions}` JSON。 |
| C6 | `M-11 压力提醒` | 默认配置未启用 `remind_drop_context_on_pressure`；启用后才追加 `drop_context_pressure`。 |
| C4（重写源） | `M-12 checkpoint 恢复` | checkpoint 不作为单独消息发送，但会替换 `Messages` 和动态订阅列表，改变下一次请求。 |

manager 记忆投影由 [`internal/coordination/stores.go`](../internal/coordination/stores.go) 完成。一个任务报告通常同时出现在：

```text
manager Messages：格式化任务报告
manager memory：task-report-* 节点
```

manager 配置了 `commit_tail_on_turn_end`；普通回合完成后，历史通常被隐藏 compact 转成记忆节点，因而不一定长期保留在 `Messages` 中。

## planner

运行时实例 ID 为 `<task-id>:planner`。planner 的工具是 `organize_subgraph`、`join`、`read`、`write`、`edit`、`ls`、`grep`、`find`、`bash`。

### 稳定性排序

| 等级 | 块 | 实际内容 |
| --- | --- | --- |
| C0 | `P-01 角色系统提示` | 调查与任务类型、变化传播边界、可验收分派判据、I1/I2/I3、执行图 / Help Executor 计划和任务类型门禁。 |
| C0/C1 | `P-02 工具 schema` | 上述九个可见工具的 name、description 和 JSON Schema。隐藏的 `inject_subscribed_memory`、`compact_memory` 不在 Tools 中。 |
| C1 | `P-03 package 结构` | 固定订阅 `<task-id>-package`，并以 `记忆：` 形式渲染。 |
| C2 | `P-04 package 契约节点` | `task-info-<id>` 的 `[Task Info] ...`；root 还会有 `[User Message] ...` 原始请求节点。helper 通常只有自己的 Task Info。 |
| C3 | `P-05 已召回记忆` | `organize_subgraph` 成功后自动订阅的新子图及其 statement。 |
| C4 | `P-06 已有历史前缀` | 之前的 planner user、assistant、工具调用和工具结果；在 compact 前通常是可追加前缀。 |
| C5 | `P-07 首条 Task 输入` | root planner 通常收到 `task.Info`；helper planner 收到 `child.Info + upstream output`。它是一条 `RoleUser`。 |
| C5 | `P-08 join pending` | 子任务候选到达时追加的 `session_id` 和 `source` 通知。 |
| C5 | `P-09 文件/命令结果` | `read` 文件文本、`ls/find/grep` 输出、`bash` 输出和退出码、`write/edit` 状态文本。 |
| C5 | `P-10 join 结果` | session 列表、候选预览、diff、文件内容、compare、apply/discard/finish 结果。 |
| C5 | `P-11 organize 结果` | `{subgraph, subscriptions}` JSON：新子图（含 `admission`/`scope`）加上本次查询对自己动态订阅的实际增删与跳过项；整理 Agent 的内部对话不会直接复制进 planner。 |
| C6 | `P-12 压力提醒` | 当前默认 planner 未配置压力提醒 hook。 |
| C4（重写源） | `P-13 checkpoint` | 恢复未完成 ReAct 的消息、工具调用 ID 和动态订阅。 |

Task Info 在 root planner 中有两份来源：package 记忆节点和第一条用户消息。Join 报告默认通过 `join` 工具进入 Messages，不是由 package 初始化代码直接追加。

## executor

运行时实例 ID 为 `<task-id>:executor`。executor 的工具是 `organize_subgraph`、`coordination_requestHelp`、`join`、`read`、`write`、`edit`、`bash`。

### 稳定性排序

| 等级 | 块 | 实际内容 |
| --- | --- | --- |
| C0 | `E-01 角色系统提示` | 最小改动、真实工具结果、help 请求、join 处理、最终 diff、验证和报告规则。 |
| C0/C1 | `E-02 工具 schema` | 上述七个可见工具的 name、description 和 JSON Schema。 |
| C1 | `E-03 package 结构` | 固定订阅 `<task-id>-package`。 |
| C2 | `E-04 package 契约节点` | Task Info；root 还包含原始用户请求节点。 |
| C3 | `E-05 已召回记忆` | executor 通过 `organize_subgraph` 召回并订阅的新子图。 |
| C4 | `E-06 已有历史前缀` | 计划、实现过程、之前的工具调用和工具结果。 |
| C5 | `E-07 planner 交接` | planner 的完整输出，作为 executor 的第一条 `RoleUser`；helper 还会叠加 child Info 和上游输出。 |
| C5 | `E-08 join pending` | 候选 session/source 通知。 |
| C5 | `E-09 文件工具结果` | 读文件、目录、搜索、写入和编辑结果。 |
| C5 | `E-10 bash 结果` | 命令正文、非零退出码、超时或错误文本。 |
| C5 | `E-11 join 结果` | 候选输出、diff、文件、compare、冲突和处理状态。 |
| C5 | `E-12 help 结果` | `coordination_requestHelp` 返回的“未提供帮助”或后续 `[join pending]` 通知；帮助任务的详细产物仍需通过 join 读取。 |
| C5 | `E-13 organize 结果` | `{subgraph, subscriptions}` JSON：新子图加上本次查询对自己动态订阅的实际增删与跳过项。 |
| C6 | `E-14 压力提醒` | 当前默认 executor 未启用。 |
| C4（重写源） | `E-15 checkpoint` | 恢复中断的实现现场、工具调用 ID 和订阅列表。 |

## verifier

运行时实例 ID 为 `<task-id>:verifier`。verifier 的可见工具集合与 planner 相同。

### 稳定性排序

| 等级 | 块 | 实际内容 |
| --- | --- | --- |
| C0 | `V-01 角色系统提示` | 目标命题 G、必要/充分条件、PASS/FAIL/INCONCLUSIVE、门禁、probe、回归和输出格式。 |
| C0/C1 | `V-02 工具 schema` | `organize_subgraph`、`join`、`read`、`write`、`edit`、`ls`、`grep`、`find`、`bash`。 |
| C1 | `V-03 package 结构` | 固定订阅 `<task-id>-package`。 |
| C2 | `V-04 package 契约节点` | Task Info；root 还包含原始用户请求节点。 |
| C3 | `V-05 已召回记忆` | verifier 主动召回并订阅的历史子图。 |
| C4 | `V-06 已有历史前缀` | 验收过程、之前的命令、工具调用和工具结果。 |
| C5 | `V-07 executor 交接` | executor 的完整输出/报告，作为 verifier 第一条 `RoleUser`。 |
| C5 | `V-08 join pending` | 候选 session/source 通知。 |
| C5 | `V-09 文件/命令结果` | 文件文本、搜索结果、命令输出和退出码。 |
| C5 | `V-10 join 结果` | 候选 diff、输出、文件、compare、apply/discard/finish 状态。 |
| C5 | `V-11 organize 结果` | `{subgraph, subscriptions}` JSON：新子图加上本次查询对自己动态订阅的实际增删与跳过项。 |
| C6 | `V-12 压力提醒` | 当前默认 verifier 未启用。 |
| C4（重写源） | `V-13 checkpoint` | 恢复验收中的 ReAct 历史和订阅列表。 |

verifier 也配置了 `commit_tail_on_turn_end`；正常完成后，剩余对话会被隐藏 compact 到记忆图。

## subgraph organizer

运行时实例 ID 通常为 `<task-id>:subgraph-organizer`。它是特殊 Agent：默认没有 `inject_subscribed_memory`，也不会把整张记忆图自动放进 SystemPrompt；当前状态主要通过整理请求的用户消息传入。

### 稳定性排序

| 等级 | 块 | 实际内容 |
| --- | --- | --- |
| C0 | `O-01 角色系统提示` | 最小节点选择、证据审核、矛盾处理、保护节点和 `memory_apply` 规则。 |
| C0/C1 | `O-02 工具 schema` | `memory_neighbors`、`memory_subgraphs_of`、`memory_sources_of`、`memory_nodes_in`、`memory_add_to_subgraph`、`memory_apply`（含 `describe_subgraph`）、`memory_subscribe`、`memory_expand`、`memory_collapse`、`memory_drop_from_context`。 |
| C0 | `O-03 organize_query 规则` | 查询是数据、选择最小相关节点、过滤 superseded/outdated、不得编造 ID 等固定规则，外加 exclude 的负向过滤语义、子图说明的写入义务和订阅裁决门槛。它会嵌入整理用户消息。 |
| C3 | `O-04 当前子图目录` | `已有子图`列表，每项一行 `id`、`kind`、`name`，随后缩进列出该子图非空的 `summary`、`admission`、`scope`。 |
| C3 | `O-05 候选节点列表` | 按 query 匹配出的节点 ID 和 statement；普通 `organizeQuery` 路径没有整体长度截断。 |
| C5 | `O-06 当前整理 query` | 调用方 query、可选的“请求方声明不需要的记忆”（exclude）、目标子图 ID、整理指令；目标 ID 是 `sg-q-N`，序号从当前图里已有的最大值往后取，因此重启后不会撞上持久化的同名子图。 |
| C5 | `O-07 普通整理用户消息` | query、目标、子图目录和候选节点的完整组合。 |
| C5 | `O-08 深度整理用户消息` | 达到阈值时提供的有界节点摘录：ID、kind/status、创建者、statement；总量限制 16 KiB，中段可能省略。 |
| C4/C5 | `O-09 organizer 长期历史` | 之前所有整理请求、assistant 回复、memory 工具调用和工具结果。当前默认没有 compact 或 turn-end commit，历史会持续增长。 |
| C5 | `O-10 memory 查询结果` | neighbors、subgraphs、sources、nodes 等 JSON。 |
| C5 | `O-11 memory view 结果` | `memory_expand` / `memory_collapse` 返回的子图、节点元数据和可选全文；可能是 organizer 最大的上下文块。 |
| C5 | `O-12 memory 写入/丢弃结果` | `memory_apply`、`memory_add_to_subgraph`、`memory_drop_from_context` 的结果 JSON。 |
| C6 | `O-13 压力提醒` | 默认 organizer 唯一启用的动态 `Suffix`；超过阈值才追加。 |

普通整理请求的构造位于 [`internal/agent/factory.go`](../internal/agent/factory.go) 的 `organizeQuery`；深度审核请求位于 [`internal/agent/curation.go`](../internal/agent/curation.go)。

## 隐藏 compact 请求

这是 manager、planner、executor、verifier 在需要时发出的第二类模型请求，不是新的长期 Agent。当前 organizer 不走普通 compact，但它可能被调用执行深度整理。

### 稳定性排序

| 等级 | 块 | 实际内容 |
| --- | --- | --- |
| C0 | `C-01 compact 系统提示` | `prompts.compact`，要求把旧对话转换为最小记忆节点。 |
| C3 | `C-02 当前订阅目录` | 当前订阅子图目录。 |
| C3 | `C-03 可选归属子图目录` | 所有可选的非 system/package 子图。 |
| C3 | `C-04 已有记忆节点` | 当前订阅节点的 ID、kind/status、statement。 |
| C4 | `C-05 待整理历史` | 用户文本、assistant thinking、assistant 文本、非 memory 工具调用和 tool result 内容。metadata 与 memory 工具调用/结果会被排除。 |
| C6 | `C-06 JSON 重试尾部` | 上一次非法 assistant 输出、`compact_json_reminder` 和解析错误；最多三次尝试。 |
| C1 | `C-07 工具集合为空` | compact 直接调用 Provider，不发送 Tools。 |

compact 元数据和历史由 [`internal/agent/compact.go`](../internal/agent/compact.go) 生成。元数据最多占约 4 KiB，整个整理输入最多约 16 KiB，超出时使用中间省略标记。

## 跨 Agent 重复关系

```text
用户消息
  ├─ manager Messages
  └─ manager memory

Task Info
  ├─ manager memory
  ├─ task package memory
  └─ planner 的第一条 user 消息

planner 输出
  └─ executor 的第一条 user 消息

executor 输出
  └─ verifier 的第一条 user 消息

verifier 输出
  ├─ manager 的任务报告 user 消息
  └─ manager memory 的 task-report-* 节点
```

## 缓存切分建议

按「静态前缀 → append-only 历史 → 易变状态 → 当轮尾部」的布局规则（见「发送顺序与前缀缓存」），最小可行的缓存边界是：

1. 静态前缀（C0/C1）：角色系统提示和工具 schema，作为不可变配置块放在 wire 最前。
2. append-only 历史（C4）：紧跟静态前缀；保留可追加前缀，原地重写只允许发生在显式的新版本切点（如 `applyCompactTail`）或尾部水位线之后——`memory_drop_from_context` 对全历史的逐条改写已按 Phase 4 收敛到最近一段，避免在压力期作废整条前缀。
3. 易变状态（C2/C3）：Task Info 契约与记忆投影按有效投影（规范化订阅集合 + 有序节点字段）缓存，协调图注入剥掉 `revision`/`executing` 后放历史之后（保留切片原有顺序，不做字典序规范化）；各 revision 字段只作失效提示，不能单独证明内容相同。
4. 当前回合与动态尾部（C5）：当前交接、工具输出和 `ModelData` 作为动态尾部，不混入固定前缀。
5. 条件性尾部（C6）：永远放在未缓存尾部。实现注意：文档早先版本只说「放在尾部」，但代码曾把压力提醒拼进 SystemPrompt 开头（wire 第 0 个 token）；已随 Phase 2 改为独立的 `Suffix` 段，物理上落在输入末尾。

审计时提出的三个收益点及落地情况：

- Responses 请求中的 SystemPrompt 双写 → 已消除（Phase 1：只保留 `input[0]` system 项）。
- manager 的完整协调图和角色的完整记忆改成独立版本块 → 已落地（Phase 2 分段 + Phase 3 稳定投影）。
- organizer 的长期历史是否需要独立 compact 或按调用隔离 → 仍然开放。organizer 默认无 compact，历史只增不减，是理论上最适合缓存的 Agent；其压力提醒 hook 在 Phase 2 后自动落到尾部段，不再打掉前缀。

## 供应商侧事实与可观测性

- 审计时 `createResponseRequest`（[internal/provider/responses.go](../internal/provider/responses.go)）没有 `prompt_cache_key`，缓存路由完全依赖前缀哈希；多 agent 并发时不同会话的请求可能被散到不同的缓存槽。Phase 0 起请求携带 `prompt_cache_key = agentID`（compact 请求为 `agentID:compact`），同 agent 的请求粘在同一路由上。
- `Store:false` + `include:["reasoning.encrypted_content"]` + assistant 输出经 `ModelData` 原样回放是无状态但**字节稳定**的续接方式，对前缀缓存友好，应保留；不要改成 `store:true` 的服务端会话（服务端会话的历史由服务端拼装，客户端失去对前缀字节的控制）。
- `CachedTokens`/`CacheWriteTokens` 早已解析进 `agent.Usage`（[internal/agent/usage.go](../internal/agent/usage.go)），但此前没有任何消费方，命中率不可测。Phase 0 起 `RuntimeEvent` 携带 `cached_tokens`，事件采集器累计，Monitor 按 `agent_id` 打印 `cached_tokens` 与 `tokens`，压测可以按 agentID 输出 cached/input 比率。
