# 并行分解方法（IPD Loop）：拆在哪一层、拆到多细、什么时候不拆

> 状态（2026-08-26）：本文把当前实现（As-Is）与目标方法（To-Be）分开记录。当前工作区的 `go test ./...` 已通过；§7 中标注「已应用」的内容是现状，标注「待定」的内容仍需单独 A/B 验证，不能当成已经启用的行为。本文本轮已把 planner 的 S0–S7 审计顺序加入 `threadmill.yaml`，但效果仍需同题 A/B 运行确认。
> 适用面：`threadmill.yaml` 的 `agents.manager` / `agents.planner` / `tools.coordination_*`，以及 `internal/coordination`、`internal/manager`。

## 1. 先确定并行到底在哪一层（代码事实）

任何拆分方法在 Threadmill 里成不成立，先取决于四条运行时事实。这些是读代码确认的，不是推测：

| # | 事实 | 位置 |
|---|---|---|
| F1 | **root 层是串行队列**。每次只取第一个 active 且 `RunPolicy=enabled` 的 root 执行，`taskRunning` 互斥；`Graph.run` 再用 `executing` 守卫返回 `ErrGraphBusy` | `internal/manager/manager.go:439-461`、`internal/coordination/run.go:76-82` |
| F2 | **并行只存在于 task 内部的辅助任务**。spawn「拉起即走，不等待」，由 join 汇合 | `internal/coordination/run.go:30` |
| F3 | **root 之间是链式继承而不是兄弟**。新 root 从前一个 root 的持久环境 fork，fork 后丢弃父 root 文件。两个 root 写同一份交付物时后者覆盖前者且不报冲突 | `internal/coordination/run.go:229` |
| F4 | **子任务级重试结构性不可行**。已开始节点不能增删关联边，已完成 task 完全不可变，因此失败的辅助任务无法重新挂回原 executor 重跑 | `internal/coordination/pending.go:127-134`、`:136` |

由 F1+F3 直接得到一条最重要的推论：**把 N 件事扇成 N 个 root 不产生并行，只产生 N 倍延迟、N 套角色开销，外加一条静默覆盖的链**。由 F4 得到第二条：verifier FAIL 之后，manager 追加 root 是当前代码下**唯一合法的出口**——修复阶段的 root 膨胀不完全是提示词没管住，是没有别的动作可选。

## 2. 诊断：问题不在第一层，在修复阶段

实测（gotmplfmt 一轮）第一层拆分是合理的：主任务 + 生产实现 / 黑盒测试 / 测试契约审查三个 helper，审查 helper 再递归请求一个边界补充 helper——这正是 A1 交付物轴 + A3 验收行轴的形状。

问题出在修复阶段：多个 root 被追加到串行队列，且没有清晰的 spawn 边。按 F1 这不是「并发混乱」，而是**串行队列里排了多个 root**；按 F3，它们还会依次覆盖彼此的交付物。所以优先级排序是：

1. **修复阶段的所有者与拓扑**（§5）——直接影响延迟与重复劳动；
2. **root 层的 wave 门控**（§6）——让「只启动当前 ready wave」成为运行时机制；
3. helper 层的粒度方法（§3、§4）——第一层已经基本对，作为维持标准使用，不是当前瓶颈。

早先版本的本文把问题判成「普遍欠分解」，依据是 DeepSWE 归因里的「287 root / 0 helper」。那是旧基线，已被这一轮观察推翻：helper 机制现在能用了，暴露出来的是修复阶段的编排问题。§3 的七轴方法**只适用于 helper 层**；用它去切 root 会正好放大 F1+F3 的代价。

## 3. 证据基础

**分解与调度：**

- *From Agent Loops to Structured Graphs: A Scheduler-Theoretic Framework for LLM Agent Execution*（arXiv 2604.11378）把 planner 失效分为五类：missing dependency、**spurious dependency**（把本可并行的节点串起来）、错误 join 选择、over-decomposition、**under-decomposition**；并指出框架**不提供自动粒度优化**，也无法在不重出 plan 版本的前提下修复其中四类。→ 粒度必须由规划者一次选对，且需要计划内自审。
- *LLMCompiler*（arXiv 2312.04511）：Planner / Task Fetcher / Executor 三段，只有依赖已满足的任务进入 ready 队列，报告最高约 3.7× 延迟收益。→ ready 队列是收益来源，`RunPolicyHeld` 就是它在 root 层的对应物。
- *DynTaskMAS*（Electronics 15(11):2475）运行时建模子任务依赖做机会式并行；*DART-LLM*（arXiv 2411.09022）依赖感知分解；*Task-Decoupled Planning*（arXiv 2601.07577）给每个子目标独立 scoped context——对应 Threadmill 的 task package + fork 环境。
- *Plan-and-Solve*（arXiv 2305.04091）先拆后逐个执行以减少漏步；*Decomposed Prompting*（arXiv 2210.02406）每个子任务有自己的提示词与验收边界并允许递归——正好是 planner → helper planner 的结构。

**执行与恢复：**

- *Runtime-Structured Task Decomposition*（arXiv 2605.15425）：由代码控制分解、状态、分支与重试，LLM 只做局部判断；失败时**只重试失败的子任务**，在一个根因分析任务上比静态拆分省 73.2% 成本。→ 这正是 F4 挡住的能力，也是本方案最值得投入的方向。
- *Effective Strategies for Asynchronous Software Engineering Agents*（CAID，arXiv 2603.21489）：中心 manager、异步执行、隔离工作区、branch/merge、测试验证——与协调图 + VFS + join 高度同构。
- *Glite ARF*（arXiv 2606.27416）：12 个并行 coding agent、273 个 task，靠确定性 verifier 脚本强制任务隔离与已完成工作不可变。「规则要活在会大声失败的代码里，而不是活在只是请求 agent 遵守的散文里。」→ I1/I2/I3 最终应下沉为工具侧校验。
- *MetaGPT*（arXiv 2308.00352）：关键不是角色名，而是 SOP 与结构化交接——每个角色产出明确工件供下游消费，而不是靠长对话。→ helper info 的七要素就是这个工件契约。

**工程方法与生态：**

- *Building Effective Agents*（Anthropic）区分 sectioning / voting / orchestrator-workers / evaluator-optimizer，并强调**连续依赖的任务不要为了并发强拆**；同系列的多 agent 实践给出显式规模档位（1 / 2–4 / 10+）与固定的 subagent 契约（objective、output format、工具与来源、边界）。
- **DSM**：聚类 + **定义接口契约来降低协调成本**。遇到写入面冲突时的正解常是**造缝**（把接口/结构/fixture 提为前置交付），而不是串行化。
- *Graph of Thoughts*（arXiv 2308.09687）：多个独立结果在唯一集成点汇合——即本文的 integration 节点。
- skill 生态：`am-will/swarms` 的 `swarm-planner` / `parallel-task`（显式 `depends_on` + wave）、`barkain/claude-code-workflow-orchestration`（依赖不确定时保守串行）都只有机制没有粒度启发式。本仓现有 skill 中最接近的是 `software-designer`、`tdd`、`golang-testing`、`golang-concurrency`、`architectural-refactor` 与 `ponytail`；它们可以组合出本方法，不需要新增工具或依赖。→ 这套方法的运行规则应写进提示词与验收文档，物理约束仍由协调图/VFS保证。

## 4. 方法：IPD Loop（**仅用于 helper 层**）

### 4.1 三条不变量

- **I1 覆盖唯一**：叶单元验收行的并集 = Task Info 累计契约全集，每行**恰好一个** owner。
- **I2 写入隔离**：同 wave 内任意两个互补单元的写入面（文件 / 符号 / 全局状态 / 配置键 / 迁移 / 生成物）交集为空。`race` 只能表示隔离的候选实现，必须在唯一裁决节点复核并最终只采纳一个，不能用它掩盖互补任务的写入冲突。
- **I3 独立可判定**：每个叶单元有单一交付物 + 一条能独立执行并给出退出码的证明命令，判定不依赖兄弟结果。

两层的执行手段不同：helper 层由 join 冲突保护兜底 I2；**root 层没有任何兜底**（F3），只能靠 manager 建图时自查。

### 4.2 循环

```
S0 契约展开   Task Info → 验收行（条件集 → 预期可观察结果）
S1 完成流程   当前状态 → 前置 → 实现产物 → 集成点 → 持久测试 → 构建 → 全量回归
S2 多轴候选   沿 ≥3 条切分轴各给一版候选，不得只给一版
S3 写入矩阵   单元 × 写入面，逐格标记，交集非空标冲突
S4 解耦决策   每个冲突三选一：合并 / 排序 / 造缝，默认优先造缝，缝进 wave 0
S5 依赖闭包   depends_on 传递闭包 → ready frontier + concurrency groups + integration
S6 叶判定     跑原子叶 checklist；非叶回 S2，只递归当前 frontier
S7 审计举证   I1/I2/I3 逐条结论；split: none 需逐轴否证
```

关键在 S2（压掉「第一想法即结论」）和 S4（把「有冲突→串行」改成「有冲突→先问能不能造缝」）。

### 4.3 七条切分轴

| 轴 | 切法 | 关系类型 |
|---|---|---|
| A1 交付物轴 | 不同文件 / 包 / 二进制 / 文档 / schema | section |
| A2 契约轴 | 一个公共 API、字段、退出码或 flag = 一个单元 | section |
| A3 验收行轴 | 等价类与正负边界分组 | section |
| A4 数据流阶段轴 | parse → transform → emit，阶段间有可观测中间产物 | pipeline |
| A5 接缝轴 | 接口 / 结构 / fixture / schema 先行，作为 wave 0 | pipeline → section |
| A6 根因假设轴 | 诊断按互斥假设并行，join 到唯一裁决节点 | hypothesis |
| A7 冗余轴 | 同契约多隔离候选，唯一节点裁决只留一个 | race |

**反轴**：按角色再派（规划 / 审查 / 列清单）；按步骤切；按文件数量凑数；把同一符号的实现与测试拆成并行单元（写入面冲突 + 循环依赖）；把 setup、文档、验证单独立任务（除非它本身就是 A1 交付物）。

### 4.4 原子叶 checklist（六条全成立才是 leaf）

1. 单一交付物，写入面与兄弟不相交；
2. 一个 reviewer 不看兄弟结果即可接受或拒绝；
3. 有一条可独立执行并给出退出码的证明命令；
4. 内部不再存在两个可独立验收的子交付；
5. 七要素（目标 / 输入与路径 / 不可违反约束 / 独立交付物 / 输出格式 / 验收证据 / 明确不做什么）都能写全；
6. 不含「必须先调查才知道改哪里」的发现型不确定——若有，它是 A6 假设单元或 `expandable`。

### 4.5 粒度档位与举证责任

| 契约规模 | target_width | 结构 |
|---|---|---|
| 验收行 ≤3 且写入面集中在一处 | 1 | `split: none` |
| 4–10 行契约，或 2–4 个不相交交付物 | 2–4 | 一次 fan-out + 1 个 integration |
| >10 行契约，或跨 ≥3 层 / 多入口 | 5–8 | wave 0 接缝 + fan-out + integration |
| 同型失败连续 2 轮 | ≥3 | A6 互斥假设并行 + 唯一裁决 |

**举证责任倒置**：提 frontier 只需给 I1/I2/I3 三条结论；判 `split: none` 必须逐条否证 A1–A7（每轴一句理由）并附写入面清单。允许判定不可拆，不允许一句话断言不可拆。

### 4.6 示例（三轮）

**Task Info**：为 `tmfleet` 增加任务级指标导出——`tmfleet metrics --format json|text` 输出各 task 的角色耗时、token、help 次数；同一份数据经 `GET /metrics` 暴露；补文档与测试；不改变现有协调图行为。

**轮 1（会被打回）**：`U1 收集器 → U2 CLI → U3 HTTP → U4 测试文档`，判 sequential。审计：12 条验收行里 11 条挂在混合体上（I1 ✗）；无一单元可独立判定（I3 ✗）；U2/U3 真正依赖的是「快照数据结构」而不是 U1 的实现——**虚假依赖**，并行被白白消灭。

**轮 2（多轴 + 造缝）**：A1 给出 cmd / handler / collector / docs 四个产物；A2 给出三个公共契约；A5 提出先定结构。S3 发现 CLI 与 HTTP 都要写格式化函数（冲突）、三者都引用尚不存在的 `MetricsSnapshot`（冲突）。S4 选**造缝**：

```
wave 0: L0 接缝（MetricsSnapshot 结构 + Collect() 签名 + format.Snapshot() 签名 + golden fixture）
wave 1: L1 collector 实现   L2 CLI 渲染   L3 HTTP 端点与状态码   ← 三者写入面互斥，各自用 fixture 断言
wave 2: L4 integration（端到端跑真实 run，三路输出一致）+ 文档
```

fixture 就是让 L1–L3 互不依赖的那条缝。

**轮 3（停）**：L1 内部的「角色耗时」与「token 用量」看似两个交付，但都写同一 struct 与同一构造路径 → 写入面相交（I2 ✗）；造缝成本 > 收益 → **合并**，标 leaf。L4 因 checklist #6 不成立，正确地留在 wave 2。

**合法的 `split: none`**：修 `internal/tool/files.go` 的 `read` 越界返回值。A1 只有一个产物；A2 只有一个契约；A3 三行验收全在同一分支且共享写入面；A4 无阶段；A5 无跨层消费者；A6 根因已知；A7 冗余无收益。写入面 = `files.go` 单函数 + `files_test.go` 单测试组。

## 5. 修复阶段拓扑（当前最该改的一处）

由 F1/F3/F4，修复阶段推荐的形状是：

```
verifier FAIL
   ↓
单个 repair root（继承原始累计契约 + 本轮已证实缺陷）
   ├── 诊断假设 A（归属：改错了入口？）
   ├── 诊断假设 B（边界：漏了等价类？）
   └── 诊断假设 C（交付链路：改动没落到项目路径？）
        ↓ 全部 join 回该 root 的 executor
   唯一 integration executor（裁决假设 → 修根因 → 跑组合门禁）
        ↓
   verifier
```

硬规则：

- **一次失败只建一个 repair root**。不要并列建生产 / 测试 / 审查 / 修复四个 root——按 F1 它们串行，按 F3 它们互相覆盖。
- **诊断并行放在辅助层**，按 A6 互斥假设切，全部 join 回同一个 executor 作为唯一集成点。
- **同型缺陷连续两轮**才转互斥诊断 wave；第一轮直接修根因即可，不要一上来就扇假设。
- 修复只针对「可由工作区改动消除」的缺陷；环境类缺陷说明复验条件，不继续扩图。

## 6. 已落地的代码改动：root 层 ready wave 门控

当前代码已经把 `RunPolicyHeld` 接入待定图、持久化和 manager 调度；`PendingRoot` 也携带 `RunPolicy`。这是一条现状能力记录，不是要求 planner 把 root 当成并行槽位：root 仍然串行，真正的并行面仍在 task 内 helper。

| 改动 | 位置 |
|---|---|
| `PendingRoot.RunPolicy`（`""` 保持现状 / `enabled` / `held`），非法值整批拒绝且图不变 | `internal/coordination/pending.go` |
| 已开始 task 的 run policy 不可变（与 info 同级保护） | `internal/coordination/pending.go` `runningSliceUnchanged` |
| 工具 schema 暴露 `run_policy` 枚举 + 串行语义说明 | `internal/coordination/graph_tools.go`、`threadmill.yaml` |
| **队头 held 挡住整条队列**：不再跳过 held root 去跑后面的（后继 root 的环境从队头 fork，越过它会 fork 到未运行的环境） | `internal/manager/manager.go` `runReady` |
| 载入旧图时 `RunPolicy=""` 归一为 enabled（否则旧状态文件里的 root 永远不会启动） | `internal/coordination/graph_store.go` |
| 协调图注入图例补上 `run_policy` 与串行语义 | `internal/coordination/graph_hook.go` |
| 测试：hold/release 往返、非法值不改图、已开始 root 拒改、旧图归一、manager 队头 held 不启动且释放后执行 | `pending_test.go`、`run_test.go`、`graph_store_test.go`、`manager_test.go` |

用法：manager 把激活条件尚未满足的后续 wave 建成 `run_policy: held` 的 root，条件满足后改回 `enabled` 释放。这让「只启动当前 ready wave」从提示词约定变成运行时机制（LLMCompiler 的 ready 队列在 root 层的最小对应物）。它不会把后续 root 变成并行任务，也不会替代 helper 层的写入隔离。

**仍然缺的**：F4 的子任务级选择性重试。RSTD 报告的 73.2% 成本下降来自「只重试失败子任务」，而当前 `rejectStartedEdge` + `completedTasksUnchanged` 使失败的 helper 无法重新挂回原 executor。要落地需要一个受控例外：允许在**已完成但未被下游消费**的辅助分支上重建 spawn/join 边，或引入显式的 `retry` 边类型。这是下一步最大的单项收益，也是唯一需要动执行路径的改动。

## 7. 提示词改动与执行计划

**已应用**：

- `agents.manager.system_prompt` 新增「编排拓扑」一节：root 串行、链式继承与覆盖风险、`run_policy=held` 的用法、修复阶段只建一个 repair root。manager 此前完全不知道 root 是串行的——这是单条性价比最高的改动。
- `agents.planner.system_prompt` 输出格式新增 `契约覆盖表`（契约原文 / 唯一 owner / 所属 wave / 验收命令），编号顺延。这是 I1 在计划层的可检查形式。
- `tools.coordination_replacePending.description`：补串行语义与 `run_policy`（与 Go 侧定义保持一致）。
- `tools.coordination_requestHelp.description`：删掉「只有单个文件或单份报告时直接完成」，改为按**写入面与验收面**判断（同一文件内多个互不相交的公共契约仍可拆；跨文件但共享符号或状态的不可拆），并加三条提交前自检。
- `agents.planner.system_prompt`：加入 IPD Loop 的 S0–S7 强制举证（契约展开、完成流程、多轴候选、写入矩阵、解耦决策、依赖闭包、叶判定、审计结论），并要求输出 `IPD 审计`；这是提示词约束，尚未宣称带来成功率或延迟收益。
- `agents.manager.system_prompt`：把 planner 的 `IPD 审计`作为 helper 准入证据，按 I1/I2/I3 优先物化当前 ready frontier 的自然单元；合并/拒绝/延后需说明证据，禁止把互补单元压成泛化 helper 或为凑宽度造任务。

**待定（本文提案，未应用）**：

- planner 拆分指南的进一步整段重写或删除旧规则。当前只追加 S0–S7，保留原有规则以便单变量回归；是否精简或重排，等 §6 的 A/B 结果再决定。
- manager 侧「帮助单元准入」（按 I1/I2/I3 校验，过粗时在物化阶段按交付物边界切分）。按 Glite ARF 的思路，这类校验更应下沉为 `coordination_provideHelp` 的服务端校验，而不是再加一段散文——前提是给帮助单元加上 `write_scope` / `contract_ids` / `acceptance_command` 结构化字段，否则无从校验。
- executor 反向条件：标为 leaf 但发现两个写入面不相交的独立交付物时，允许按证据提交一次拆分请求。

### 7.1 按计划落地顺序

这套方法先做提示词与观测 A/B，不先改协调图语义：

1. **冻结基线**：保存当前 `threadmill.yaml`、二进制、协调图、记忆图、runtime log 和候选产物；记录模型、题目、提交版本与环境。
2. **离线计划审查**：让 planner 只输出 S0–S7 计划，不执行代码。检查 I1/I2/I3、契约 owner、写入矩阵和 ready frontier；不合格计划直接重规划。
3. **同题控制运行**：用现有提示词跑一批，统计 helper 数、frontier 宽度、join 冲突、repair root 数、端到端时间和 token。
4. **单变量实验**：以当前已追加 S0–S7 的 planner 文案作为 B 组，与冻结的旧 planner 文案 A 组对照（或只启用 manager 的单 repair-root 文案）；不要同时改 VFS、调度和记忆压缩。
5. **合流验收**：executor 每个 wave 只提交一次帮助请求，所有 helper join 后由唯一 integration 节点运行组合门禁；verifier 按契约表逐行报告。
6. **失败处理**：只有 verifier 给出可复现、可由工作区改动消除的缺陷时才追加一个 repair root；上游超时、Docker、权限和 benchmark oracle 问题只记录复验条件，不扩图。
7. **停止条件**：连续两批满足覆盖率 100%、同 wave 冲突 0、独立证明覆盖 100%，且关键路径/token 不劣于基线，才保留提示词改动；否则回退实验性文案，不改架构语义。

本轮实现证据：`go test ./internal/provider -run 'TestRepositoryPlannerPrompt(UsesIPDLoop|BatchesReadyWork|TreatsRepairRootsIncrementally|RechecksUpstreamClaims)$' -count=1` 通过；该测试只检查配置中存在方法约束，不代表模型已经按方法执行。

第一版不增加工具。`coordination_requestHelp` 的 `reason` 先承载七要素、写入范围和证明命令；只有在多批实验确认模型经常伪造或遗漏这些字段时，才考虑把字段结构化并下沉到服务端校验。

## 8. 验证指标

| 指标 | 现状 | 目标 | 说明 |
|---|---|---|---|
| 关键路径上的串行 root 数 | 修复阶段 4+ | 每次失败 1 个 repair root | 最能暴露 F1 代价的量 |
| 每 root 的 helper 数与 frontier 宽度 | 第一层 3–4 | 维持；修复阶段 ≥2（假设并行） | 只在 helper 层有意义 |
| helper 层 join 冲突率 | —— | ≈0 | 非 0 说明 S3/S4 的写入矩阵是走过场 |
| 同型缺陷重复修复轮数 | 连续同构追加 | ≤2 后转互斥诊断 | |
| 端到端延迟与 token | 记录基线 | 不劣于基线；关键路径尽量下降 | root 串行下，root 数几乎线性影响延迟 |

A/B 建议：使用同一批任务比较 A（当前 prompt）与 B（只启用 §7 待定的 S0–S7 文案），看上表指标、最终任务结果与 token 成本。§6 的 root 门控单独作为运行时基线，不和 planner 文案实验混为一个变量。

## 9. 参考

- [From Agent Loops to Structured Graphs (arXiv 2604.11378)](https://arxiv.org/html/2604.11378v1)
- [LLMCompiler (arXiv 2312.04511)](https://arxiv.org/abs/2312.04511)
- [Runtime-Structured Task Decomposition (arXiv 2605.15425)](https://arxiv.org/abs/2605.15425)
- [Effective Strategies for Asynchronous Software Engineering Agents / CAID (arXiv 2603.21489)](https://arxiv.org/abs/2603.21489)
- [Glite ARF: Verifier-Driven Research with Parallel LLM Coding Agents (arXiv 2606.27416)](https://arxiv.org/abs/2606.27416)
- [Toward Scalable LLM-Based Multi-Agent Collaboration / DynTaskMAS](https://www.mdpi.com/2079-9292/15/11/2475)
- [Beyond Entangled Planning: Task-Decoupled Planning (arXiv 2601.07577)](https://arxiv.org/pdf/2601.07577)
- [DART-LLM (arXiv 2411.09022)](https://arxiv.org/pdf/2411.09022)
- [Plan-and-Solve Prompting (arXiv 2305.04091)](https://arxiv.org/abs/2305.04091)
- [Decomposed Prompting (arXiv 2210.02406)](https://arxiv.org/abs/2210.02406)
- [MetaGPT (arXiv 2308.00352)](https://arxiv.org/abs/2308.00352)
- [Graph of Thoughts (arXiv 2308.09687)](https://arxiv.org/abs/2308.09687)
- [Building Effective Agents — Anthropic](https://www.anthropic.com/engineering/building-effective-agents)
- [Introduction to DSM](https://dsmweb.org/introduction-to-dsm/)
- [am-will/swarms](https://github.com/am-will/swarms) · [barkain/claude-code-workflow-orchestration](https://github.com/barkain/claude-code-workflow-orchestration)
- [Writing Plans skill — obra/superpowers](https://github.com/obra/superpowers/blob/main/skills/writing-plans/SKILL.md)

本仓对应 skill：`software-designer`（契约与依赖图）、`tdd`（先写叶任务验收）、`golang-testing`（独立证明命令）、`golang-concurrency`（frontier 与并发安全）、`architectural-refactor`（按接缝分阶段）、`ponytail`（拒绝伪拆分）。这些是执行方法的参考，不改变 Threadmill 的角色边界。
