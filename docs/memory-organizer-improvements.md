# Memory Organizer 上下文压缩、图维护纪律与 Benchmark 改进

## 文档信息

| 项目 | 内容 |
| --- | --- |
| 版本/状态 | v0.2 / 已实现（待真实运行验收） |
| 日期 | 2026-08-31 |
| 责任角色 | agent 起草，人类评审 |
| 目标读者 | Threadmill 提示词与协调图维护者、benchmark 评估者 |
| 形成方式 | 由 `benchmarks/memory-organizer` sequential 运行（`gpt-5.6-luna`，2026-08-31，artifacts 在 `/tmp/threadmill-organizer-eval.7u5iSx/sequential-results-luna-v2`）暴露的问题驱动；机制事实由代码逆向核对 |
| 人类给定方向 | ①压缩应利用三层展开机制而非一般记忆压缩；②核实 agent 是否真用提供的工具与展开；③教 organizer 高效维护/检索图，子图语义原子但不必正交，节点可多归属，避免订阅带入不相干节点；④评估 query 情景分散度；⑤产出改进文档 |
| Agent 增补 | 运行证据的量化分析、harness 修正项（对照模型一致性、退化打标、指标拆分、artifact 瘦身）、优先级与验收 |

### 修订记录

| 版本 | 日期 | 修订人/责任角色 | 修订内容 | 状态 |
| --- | --- | --- | --- | --- |
| v0.1 | 2026-08-31 | agent | 初稿 | 草案 |
| v0.2 | 2026-08-31 | agent | 按 §6 优先级实现 P0/P1/P2，回填开放问题决定 | 已实现 |

## 1. 背景

`benchmarks/memory-organizer` 把 env-30 的记忆图（331 节点、33 子图）克隆进评测环境，保持一个 organizer 会话连续回答 6 个查询，测量"整理工作是否跨查询复利"。Luna v2 运行（6 查询、35 次模型调用、9.3 分钟、9.24M 输入 token、缓存命中 91.6%）机制上成功：Q1 合规退订 sg-q-1/sg-q-2、精确翻转 mem-164/165 为 outdated；Q2–Q6 建出描述完整的 sg-q-4…sg-q-8。同时暴露了本文档要处理的产品与 benchmark 问题。

关键运行证据（后文引用处不再重复来源）：

| 证据 | 数据 |
| --- | --- |
| 上下文单调增长 | 单次调用 input 从 12.4k（Q1 首次）涨到 561.8k（Q6 末次）；配置窗口 128k |
| organizer 无 compact 路径 | `NewSubgraphOrganizer` 只装 MemoryTools+memory_subscribe（`internal/agent/factory.go:254-276`）；`shouldCompactBeforeRequest` 要求工具表含 `compact_memory`（`internal/agent/loop.go:454-465`），条件永不满足 |
| 词汇匹配候选精度低 | 查询消息内匹配候选 44–250 个/查询，最终选中 2–47 个 |
| 跨查询发现依赖对话历史 | legacy-cleanup 选中 mem-164/175/177、version-abi 选中 mem-161/46/122，均不在当次匹配候选内，来自前序查询的对话记忆 |
| 展开纪律不稳 | probe-reconfiguration 选 47 节点零 L3；ctest-matrix 选 16 节点零 L3；install-consumers 先 add_to_subgraph 36 个再补 L3；collapse 全程 1 次 |
| 图导航工具零使用 | memory_neighbors / memory_subgraphs_of / memory_sources_of / memory_nodes_in 六查询无一调用 |
| drop 挡不住增长 | drop_from_context 每轮主动清数十节点，但受保护前缀（`internal/agent/drop_context.go:92-95`）外消息不可改写 |
| 子图近乎正交 | 6 张子图两两 Jaccard ≤ 0.08，仅 mem-109/120/155 三个节点多归属 |
| 工具错误 2 次（自愈） | `memory_apply` 缺 reason；幻觉节点 `env-25-mem-136` 被工具正确拒绝 |

## 2. 产品问题一：organizer 的上下文压缩

### 2.1 为什么不能走一般记忆压缩

`compact_memory` 的产出是新 `mem-N` 节点**追加进调用 agent 绑定的同一张图**（`internal/agent/hidden_tools.go:113-127` → `internal/agent/compact.go:32-76`），只增不改。给 organizer 注册它会产生三个问题：

1. **循环污染**：用 organizer 正在整理的图充当其对话草稿纸；benchmark 场景下直接污染评估对象。
2. **语义错位**：compact 产物只增不改，而冲突裁决、状态翻转本是 organizer 深度整理的职责（`threadmill.yaml:589-599`）。
3. **解决错对象**：organizer 上下文的增长源是跨查询累积的**消息历史**（organizer 无 StateBlocks，每查询一条 user 消息 + 工具往返），不是记忆块膨胀；compact 压缩对话进图，图又会以更臃肿的形态回到后续上下文。

结论：人类给定方向成立——organizer 的压缩不应复用 `compact_memory`。

### 2.2 用三层展开机制做"会话重置 + 图重实例化"

三层展开（L1 子图说明 → L2 节点元数据 → L3 statement 全文，`internal/agent/memory_view.go:13-19`）使 organizer 的工作状态可以**从图重新实例化**：子图目录本就在每次查询消息里重新嵌入（`internal/agent/factory.go:1013-1020`），节点经 expand 按需取回。这意味着 organizer 的持久状态应当全部住在图里（子图描述、节点状态、归属），消息历史只是可丢弃缓存——**图就是记忆，历史即缓存**。

提议机制（按保守程度排序）：

- **方案 A（阈值重置）**：organizer 估算 token 达到 `softContextThreshold`（`internal/agent/drop_context.go:161-163`，3/4 窗口）时，不 compact，而是开启新会话：system prompt + 当前图子图目录 + 当次查询与匹配候选 + **结构化交接**（本 task 内创建/修改过的子图 ID 清单与一句话状态，数百 token 量级）。工具结果与推理链丢弃。
- **方案 B（按查询重置）**：每次 `organize_subgraph` 调用即新会话。最简单、成本下界最低，即 benchmark cold 模式的产品化。
- **方案 C（维持现状 + drop/collapse）**：已证伪——drop 受保护前缀限制（证据见 §1），增长不可控，真 128k 窗口模型在第 3 个查询就会失败。

推荐方案 A：保留短程推理连续性，又把单次调用成本上界压到"目录 + 工作集"量级。方案 B 作为 A 的极限情形由 benchmark 直接对照（见 §5.3 同模型 cold 对照）。

### 2.3 重置的真实损失与补救：判断必须落图

纯重置会丢失跨查询发现通道——本次运行中 organizer 靠对话历史想起匹配候选之外的 mem-164/175/177（legacy-cleanup）与 mem-161/46/122（version-abi）。补救不是保留历史，而是**把判断外部化到图**：

- 排除理由落图：Q1 排除 mem-161 时，若在 sg-q-3 的 scope 写明"ARM/toolchain 腿未覆盖、mem-161 维持 disputed"，version-abi 无需对话记忆即可经目录重新发现。
- 交接清单落图：结构化交接（§2.2 方案 A）保证"我建过哪些子图"可恢复。
- 这也是 §3 提示词增补的同一件事：**判断不落图，历史不可替代；判断落图，会话即可丢弃**。

## 3. 产品问题二：工具与展开的真实使用 + 图维护纪律

### 3.1 agent 会不会用提供的机制：会用，但纪律不稳

肯定的部分：6/6 查询走 L1→L2 渐进展开；Q1 严格 1→2→3；`drop_from_context` 每轮主动清理；退订理由照抄请求方声明（合规）；词汇匹配之外能经子图展开做发现。

否定的部分（§1 证据）：两个查询未读 statement 全文就定了 47/16 个节点的归属；install-consumers 先定归属后补 L3；collapse 仅 1 次；四个图导航工具零使用。提示词已写"严格按级别 1 → 2 → 3"（`threadmill.yaml:569`），但无强制，靠自律。

处置（评测先行，产品随后）：

1. benchmark 增加质量指标：**选中节点中定归属前未达到 L3 的比例**、导航工具使用率、collapse/drop 频次。先让问题可见，不改生产行为。
2. 产品侧候选（待人类批准）：`memory_add_to_subgraph` 对当前上下文未达 L3 的节点返回非阻断警告，把"元数据定归属"变成可见事件。

### 3.2 教 organizer 高效检索

现状：发现过度依赖查询消息的词汇匹配候选（`nodeMatchesQuery`，子串/token≥4 字符，`internal/agent/factory.go:1021-1063`）。提示词增补方向：

- 匹配候选少而查询主题宽时，主动对相关**既有子图**做 L2 展开扫节点头，而非只扫消息候选列表；
- `memory_neighbors` 用于前沿补全（现提示词仅"只在补足关系时读邻居"，零使用——考虑改成正面触发条件示例）；
- 判断与排除理由经 `describe_subgraph` 落图（配合 §2.3）。

### 3.3 子图语义：原子，不必正交；节点可多归属

数据模型完全支持人类给定方向：`SubgraphIDs` 注释"一个节点可属于多个子图"（`internal/context/graph.go:44`），全代码无单归属/正交约束。现状瓶颈在提示词与行为：提示词只说"最少归属、不改其他归属"（`threadmill.yaml:571`），本次 6 张子图近乎完美正交，而 version-abi 又回头捞 mem-109/161——多维意义的需求真实存在，动作通道却不顺。

提示词增补方向：

- **原子而非正交**：一张子图回答一个问题/契约；不同子图可共享节点，禁止为正交而复制节点或硬塞归属。
- **多归属优先**：同一节点服务多个问题时同时归属多张子图；允许 `memory_add_to_subgraph`/`memory_apply attach` 增补**非目标**子图的归属（放宽"不改其他归属"为"可增补、不得擅自移出"）。
- admission/scope 分工不变：admission 约束"什么节点能进来"，scope 约束"谁在什么时候该订阅"（`internal/context/graph.go:57-59`），多归属下 scope 更要把失效信号写清。

### 3.4 订阅带入不相干节点：责任分配与注入块标注

订阅者的注入块只有 `- [kind/status] statement` 扁平列表，**无子图来源、无 admission/scope**（`internal/agent/input.go:232-248`），订阅者无法自判相关性。因此"不带入不相干节点"当前 100% 依赖 organizer 成员精度与订阅裁决。两个选项：

- **选项一（推荐先测）**：注入块按子图分组渲染并标注子图名/来源（多归属节点标注全部来源），让订阅者能看到边界、自行降权不相干节点。成本：格式变长、缓存粒度变细、多订阅去重语义变化。
- **选项二（维持现状）**：保持扁平渲染，用 benchmark 的"无关节点率"指标盯 organizer 成员精度。

建议 benchmark 对选项一做一轮 A/B 后再定产品默认值。

## 4. Benchmark 问题一：query 情景分散度

### 4.1 现状评估

主题划分合格（清理/探针/安装/测试/版本五条线，跨 env-*-mem-* 来源），但情景类型单一：6 个查询同属一个项目，5/6 同构（召回 + 建子图），成员近零重叠。以下情景完全未覆盖：

| 缺失情景 | 测什么 | 现状 |
| --- | --- | --- |
| 负控制（图中无相关主题） | 提示词承诺"无相关节点时选择 0 个"（`threadmill.yaml:573`）；区分"正确空集"与 grok 冷启动式退化 | 未测 |
| 已有子图部分失效 | 三档退订裁决：整图退订 / detach 少数节点 / 不动（`threadmill.yaml:583-587`） | 仅 Q1 测了整图退订 |
| 多维归属 | 两个查询天然共享节点（如 ABI 等价与源码覆盖共享对象构建证据），断言同节点进两图 | 未测，且本次行为近零重叠 |
| 深度整理/去重 | curation 路径（`internal/agent/curation.go`）、重复节点合并 | 完全未覆盖 |
| 规模/压力 | 更多节点或更多连续查询，压出会话重置行为（§2.2） | 未测 |
| 订阅者视角 | 退订后注入投影确实消失、目标子图不可退（`internal/agent/factory.go:938-952`） | 只校验了 driver 侧订阅列表 |

### 4.2 处置

保留现有 5 个控制查询 ID 不变（README 已要求的冷启动对比前提），新增"扩展组"查询单独标记；为每条查询在 `queries.json` 增加可机判断言（见 §5.2）。

## 5. Benchmark 问题二：harness 修正

### 5.1 对照有效性

- **cold 对照必须用同模型**。现有 cold run 为 grok-4.5（sequential 为 luna），且输出退化（5/5 子图 admission/scope 为空、3/5 零成员、4/5 summary 照抄 query）。`report.sh` 在两个 summary 的 `model` 字段不一致时应拒绝对比或显著告警。

### 5.2 质量轴（从"观察"变"测量"）

- `queries.json` 每查询增加可选断言：`must_include` / `must_exclude`（节点 ID）、`expect_min_selected` / `expect_max_selected`。driver 自动判定并在报告中单列失败项。
- 退化打标：selected=0、admission/scope 缺失、summary 与 query 原文相同、subgraph revision=0，任一命中即在 case 结果与报告中标记（不阻断运行）。
- 新增 §3.1 的纪律指标（未 L3 定归属比例、导航工具使用、无关节点率）。

### 5.3 指标与 artifact 修正

- `memory_organizer_candidates` 当前是全图节点数（`internal/agent/factory.go:983-1002`），报告中误读为"候选"；改为记录查询消息内真实匹配候选数。
- `report.sh` 的 `node_ops` 把 `subgraph_ids` 成员变更与状态/内容变更混算；拆成"归属变更 / 状态变更 / 内容变更"三列——本次全程真实状态变更只有 2 次（mem-164/165），现有报告看不见。
- 工具错误计数（metrics 已有 `tool.errors`）进入报告表格。
- `summary.json` 瘦身：50MB/6 查询且 O(n²) 增长（完整 request/response 同时存于 per-case 文件与 summary）。summary 只留指标与 case 文件引用，全量 trace 仅留 per-case 文件；末轮 `summary.partial.json` 与 `summary.json` 去重。

## 6. 优先级与验收

| 优先级 | 项 | 验收 |
| --- | --- | --- |
| P0 | organizer 会话重置（§2.2 方案 A） | 同 workload 下单次调用 input 峰值有界（不超过目录+工作集量级）；6 查询总 uncached token 较现状 777k 下降；图终态（子图/状态）与现状运行可比 |
| P0 | 同模型 cold 对照 + report.sh 模型一致性检查（§5.1） | cold/sequential 同模型各跑一次，报告可直接对比；模型不一致时 report.sh 告警 |
| P1 | queries.json 断言 + 退化打标（§5.2） | Luna v2 artifacts 重放可判：sg-q-3 缺 ARM 腿（mem-161）应被 must_include 类断言捕获 |
| P1 | 提示词增补：非正交/多归属/检索纪律/判断落图（§3.2-3.3） | 多维归属场景（新增扩展组查询）中共享节点进入 ≥2 子图；未 L3 定归属比例下降 |
| P2 | 订阅注入块归属标注 A/B（§3.4 选项一） | 订阅者上下文 token 成本与无关节点率均有数据后再定默认值 |
| P2 | 指标与 artifact 修正（§5.3） | summary.json 体积降至 MB 级；报告含状态变更列与工具错误列 |
| P2 | 扩展组情景（§4.2） | 负控制、部分失效、多维归属、深度整理至少各 1 条 |

## 7. 开放问题（v0.2 决定）

- **会话重置的交接 schema**：决定只带机械可得的信息——本次会话经手过的目标子图 ID，加上它们在图上的 name/summary/admission/scope。**不**让 organizer 为交接额外写一次小结：那要多一次模型调用，且小结本身是新前缀、会作废整段缓存，而它想保住的东西（未决判断）本来就该经 `describe_subgraph` 落图。提示词因此同时增补了“判断落图”一节：没写进图的判断，重置后就该丢。
- **多归属的去重语义**：不需要按“首次归属”计权。`memory_organizer_selected` 数的是本次目标子图的成员，增补别的子图的归属不进这个计数；订阅投影走 `Graph.NodesInSubgraphs`，本身按节点 ID 去重（`internal/context/graph.go:93-99`），多归属不会让订阅者多看到一份。评测另出 `shared_with_earlier`（与指定的更早查询选中集合的交集大小）来正面测多维归属，而不是从 selected 里推。
- **`EnvView.Commit` 无 CAS**：本次不动。会话重置是纯对话侧操作，不写图，没有新增提交冲突面；多角色共用图的 CAS 需求与本文档的改动正交，留在原议题里单独验证。

## 8. v0.2 落地清单

| 项 | 实现 |
| --- | --- |
| P0 会话重置（§2.2 方案 A） | `internal/agent/session_reset.go`：估算 token 过 `softContextThreshold` 时，把消息截到当前回合起点并在最前面放一条从图渲染的交接消息；截点取“最后一条真正的 user 消息”，当前回合内的 tool_call/tool_result 配对保持完整，因此回合之间与整理中途都能重置。挂在 `Loop.generate` 里 compact 判断之前（`internal/agent/loop.go`），只对整理 Agent 开启（`NewSubgraphOrganizer` 与 `newFileLoop` 的 organizer 分支），`FileOverlay.NoSessionReset` 供评测做 A/B。交接抬头可由 `prompts.session_reset_handoff` 覆盖。 |
| P0 同模型对照 | `report.sh` 在两个 summary 的 `model` 字段不一致时以退出码 3 拒绝对比；对比只取 control 组。 |
| P1 断言与退化打标（§5.2） | `queries.json` 每条查询可带 `must_include`/`must_exclude`/`expect_min_selected`/`expect_max_selected`/`must_stay_subscribed`/`must_share_with`；driver 判定后写进 case 与报告，不阻断运行。退化打标覆盖 selected=0、admission/scope 缺失、summary 照抄 query、subgraph revision=0。 |
| P1 纪律指标（§3.1） | driver 重放工具调用还原视图级别，产出 `membership_commits_without_l3`、导航工具调用数、expand/collapse/drop 次数、会话重置次数、单次调用 input 峰值。产品侧的“未达 L3 归属告警”仍待人类批准，未实现。 |
| P1 提示词增补（§3.2-3.3） | `threadmill.yaml` 的 organizer 系统提示与 `prompts.organize_query`：候选列表不是节点全集（宽查询先对既有子图取级别 2）、导航工具的正面触发条件、子图原子而非正交与多归属（归属只增不擅自减，移出只走 detach）、定归属前取级别 3、判断与排除理由落图。`internal/provider/config_test.go` 的提示词字节预算相应上调并写明理由。 |
| P2 注入块归属标注 A/B（§3.4 选项一） | `agent.FormatSubscribedMemory(graph, subs, attribution)`：分组渲染标注来源子图与多归属，隐藏工具按 `FileOverlay.SubscriptionAttribution` 选渲染方式（默认仍是扁平）。driver 每个 case 记录两种渲染的字节数（`projection_cost`），A/B 数据先到位再定默认值。 |
| P2 指标与 artifact（§5.3） | `memory_organizer_candidates` 改记查询消息内真实匹配候选数（`countQueryMatches`）；`report.sh` 把 node_ops 拆成 attach/status/content 三列并新增 tool_errors 列；summary 里的 case 去掉 request/response 与事件全量（只留 `case_file` 引用），末轮删掉 `summary.partial.json`。 |
| P2 扩展组情景（§4.2） | `queries.json` 新增 `group: extended` 的负控制、部分失效（须 detach 且不退整图）、多维归属（须与 version-abi 共享节点）、深度整理（`mode: curate`，经导出的 `agent.RunDeepCuration` 走生产路径）。control 组 ID 与顺序由 `main_test.go` 锁定。 |

未做（有意）：§3.1 提到的 `memory_add_to_subgraph` 未达 L3 非阻断警告——文档写明待人类批准，评测已能让该行为可见，先测再改生产。

验收仍需一次真实运行：P0 的“单次调用 input 峰值有界、6 查询总 uncached token 下降”和 P1 的断言重放都只能由带模型的运行给出数据，本次改动只保证机制与可测量性。
