# Threadmill 模型提示词设计

> [`threadmill.yaml`](../threadmill.yaml) 是可配置提示词的来源；代码 fallback 和动态控制文本另列在下文。本文不复制全文，避免与实际请求漂移。

## 1. 实际输入层次

模型请求按稳定到易变排列：

1. 角色 system prompt：`prompts.default` 兜底，或某个 `agents.*.system_prompt`；配置缺失时还有代码 fallback。
2. 受保护 task package：root 含创建请求和 Task Info；helper 只有自身 Task Info。helper 还可能看到上游输出和继承记忆，但它们只是线索，不能补权限。
3. 追加式 user / assistant / tool 历史。
4. 记忆视图、协调图等可替换状态块。
5. 上下文压力等当轮 system 尾段。
6. 当前角色实际拥有的工具定义。

`prompts.default` 不是公共前缀；正常角色各自收到完整专用 prompt。工具参数、副作用和错误恢复归 description/schema 所有。完整发送点如下：

- 配置：[`threadmill.yaml`](../threadmill.yaml) 的 5 个 `prompts.*`、5 个 `agents.*.system_prompt`，以及实际启用工具的 `description`/schema。
- 无配置 system fallback：[`internal/agent/loop.go`](../internal/agent/loop.go) 的 `DefaultSystemPrompt`。
- 压力提醒 fallback：[`internal/agent/drop_context.go`](../internal/agent/drop_context.go) 的 `dropContextPressureReminder`。
- 普通记忆整理动态包装：[`internal/agent/factory.go`](../internal/agent/factory.go) 的 `organizeQuery`。
- 深度整理动态请求：[`internal/agent/curation.go`](../internal/agent/curation.go) 的 `deepCurationQuery`。
- task package、图/记忆状态和上游交接由运行时代码生成；见 [`model-context-blocks.md`](model-context-blocks.md)。

## 2. 所有完整 system prompt 的共同结构

每份提示词开头一句话说明这个角色在干什么，随后是若干 `## ` 分节，`## 输出` 收尾。分节按**活动**命名，不按模板槽位命名：

| 角色 | 分节 |
| --- | --- |
| manager | 授权与可信输入 · 分类输入 · 建 root · 响应拆分请求 · 审计 verifier 报告 · 改图规则 · 输出 |
| planner | 授权与可信输入 · 调查工作区 · 写累计契约 · 语义精化与知识边界 · 继续精化、委派，还是直接实现 · 生成执行图 · 选门禁 · 规划修复 · 输出 |
| executor | 授权与可信输入 · 开工前 · 什么时候请求帮助 · 实现 · 证据与门禁 · 处理 join 候选 · 卡住时 · 输出 |
| verifier | 授权与可信输入 · 建验收表 · 选门禁 · 充分性检查 · 证据账本 · 读 diff 与 join · 定 verdict · 输出 |
| organizer | 边界 · 先识别模式 · query 模式 · 深度整理模式 · 工具用法 · 输出 |

这替换掉了原来的 outcome-first 模板（`职责与边界` / `成功标准` / `方法` / `输出`）。模板的问题出在 `方法`：它是一个编号杂物袋，调查、委派、证据、join、失败恢复混在一起，模型要扫完全部条目才能找到管着当前动作的那一条，而每条的判据、触发条件和例子又散落在别的段里。按活动分节之后，模型可以直接跳到当前动作对应的那一节，触发条件和例子就在规则旁边——这正是 Codex 用 `## Planning` / `## Task execution` / `## Validating your work` 的原因。

原 `成功标准` 的内容没有删，而是分配进了产生它的那一节：契约覆盖率归 planner 的`写累计契约`，DONE 的证据要求归 executor 的`证据与门禁`，PASS/FAIL/INCONCLUSIVE 归 verifier 的`定 verdict`。判据和产生判据的动作放在一起，而不是隔着两屏。

写作约束（全部按 Codex harness 的写法）：

- **一句话一个意思。** 不写分号串起来的五段式长句。
- **先正面说怎么做，再补护栏。** 护栏是一两句，不是一张禁令表。
- **自由裁量的决定给处境清单**，写成 `在下列情况 X：`，列可辨认的处境，不列要证明的门槛。
- **例子紧跟它所服务的那一节**，用同一场景的正反对照，让对比承担教学。
- 一条规则只在其所有者处出现；角色策略归角色，工具协议归工具。
- 不用“高质量、合理拆分、充分验证、尽可能”等词代替判据。若保留术语，紧邻定义可观察含义。
- 不按固定 agent 数、文件数或步骤数决定并行度。
- 静态契约在前，动态任务、图、记忆和压力提醒在后。

工具 description 同样按这个顺序写：意图 → 处境清单 → 护栏 → 正反例 → 字段与协议。机械字段永远排在最后，因为它们回答的是“怎么填”，不是“要不要做”。

两份控制提示词（`drop_context_pressure`、`organize_query`）和 `compact_json_reminder` 只有几句话，不分节，写成直接的祈使文本。

改写覆盖全部 8 份 system prompt（含 [`internal/agent/loop.go`](../internal/agent/loop.go) 的无配置 fallback）、3 份控制提示词，以及 `coordination_requestHelp`、`coordination_orchestrate`、`coordination_publishTask`、`bash` 四份承载判断的工具 description。任务角色只用 `coordination_requestHelp` 提交编排建议；Manager 只用 `coordination_orchestrate` 的 `replace_pending` / `provide_help` 动作改图。`coordination_publishTask` 把 task 终态、Verifier verdict 与真实项目发布分开：Manager 只选择一个已归档快照，工具成功后才宣称落盘。

[`internal/provider/config_test.go`](../internal/provider/config_test.go) 的 `TestRepositorySystemPromptsUseTopicalSections` 检查：开头是一句角色陈述、至少 3 个分节、分节名不重复、`## 输出` 收尾、旧模板段落不得回潮，以及总字节预算。其余静态测试检查角色不变量、分派边界，以及真实验收项目没有覆盖内置提示词。

## 3. 任务类型

| 类型 | 完成状态 | 最少证据 | 不适用动作 |
| --- | --- | --- | --- |
| 结论型 | 无授权持久交付的纯结论任务：结论可由证据推出，相对 task 继承基线零新增持久写且不回退 | 原始来源、复现、代码路径或等价直接证据 | 未经要求修改工作区 |
| 行为变更型 | 约定条件产生约定结果 | 契约证据；代码有稳定入口时加持久回归，文档/配置做 parse/render/schema/reference 检查，产物参加构建时才构建 | 与契约无关的重构 |
| 行为保持型 | 目标结构变化，明示行为不变量仍成立 | 结构检查、不变量/兼容性检查、受影响回归 | 用内部形状代替行为证据 |
| 评测型 | benchmark 数据、harness/基线、环境、模型非秘密配置、评分协议和日志冻结，结论可复算 | 原始日志、样本结果、聚合方法和版本；submission 只按 Task Info 修改 | 未授权干预 submission 或评测协议 |

类型不是四选一：每条契约可有多个类型，一个任务可同时包含多类，门禁取并集；不能为了类型标签另造 task。

修复不是第五种类型，而是处理方式：复现 → 找到最早责任边界 → 用单变量实验证伪根因 → 最小修根因 → 在最后一次改动后复验累计契约。

门禁是否适用只有一个判据：该门禁失败能否证伪某条 Task Info 条件，或证伪本次 diff 触及的既有行为。不能则记录不适用理由，不把它用作阻断证据。

### 通用报告协议

报告不再用“是否出现退出码”代表可信度。Verifier 对每条累计契约输出一条证据记录：

| 字段 | 含义 |
| --- | --- |
| `契约` | Task Info 中的 Ci |
| `状态` | `PASS`、`FAIL` 或 `UNVERIFIED` |
| `主张` | 本条证据实际支持的可观察结论 |
| `证据锚` | 另一角色能够精确重取证据的位置 |
| `原始观察` | 与主张直接相关的原始结果 |
| `适用范围` | 版本、环境、时间或状态边界 |

证据锚不限定媒介：开发任务可以是原命令与退出码，研究任务可以是 URL/文档 ID 与章节，文件或产物可以是路径、符号或哈希，运行事件可以是日志 ID 与时间范围。只有概括、二手转述或裸链接都不是完整记录；没有证据锚时必须写 `UNVERIFIED`。

这套协议只统一“怎样交付可复核主张”，不把四种状态混成一个：task outcome 说明流程是否终止，Verifier verdict 说明累计契约是否成立，证据记录说明每条主张凭什么成立，`publishTask` 说明哪个终态文件快照真正落盘。报告仍是自报，Manager 负责审计，不因格式完整自动认证。

记忆投影也按同一边界处理：没有 Verifier verdict 的运行时错误是系统观察；带 verdict 的报告必须至少含一条完整证据记录才进入 `accepted`，否则为 `disputed`。旧报告中的命令与退出码继续兼容，但不再是唯一证据形状。

## 4. 语义精化

抽象不是预先猜未来功能，而是把压缩的目标递归展开成可执行语义：

1. 说明目的和“条件 → 可观察结果”。
2. 说明承诺、非承诺、不变量、需要下层提供的能力、知识 owner 和实现无关的验收。
3. 用稳定、正交、最小而完整的概念保留任务信息，不把当前机制的偶然形状升级为契约。
4. 把因同一原因变化的决定放在一起。上层拥有策略与语义约束，下层拥有机制。
5. 只有语义变化穿过知识边界；若内部实现变化迫使兄弟修改，边界仍未形成。

未形成契约的抽象不得继续向下实现；那不是委派，而是歧义传播。

## 5. 认知闭包与分派

对每个节点问三件事：上层是否知道如何使用它，是否知道如何验收成败，并且无需知道内部实现也能完成上层决策。

- 任一项不成立：继续语义精化。
- 全部成立、只是简单叶子或成熟原语：直接实现或复用。
- 全部成立、边界能独立说清并独立验收：**委派**。

这叫认知闭包。它是双向判据，不是单向收紧：不闭合时不得向下实现，闭合而边界已独立成立时也不得自己继续钻。第三条既不是规模阈值也不是难度门槛——判据是「把内部装进本层之后，本层还剩多少判断力」，所以既不需要为了触发它去清点，也不必等一个子问题大到自成学科才肯交出去。扩展性和并行度是正确抽象的副产品，不是需要追求的数字；但边界一旦成立，卸载就是义务而不是可选项。认知闭包决定“能否委派”，下层实现域规模决定“是否必须委派”，工具协议再决定如何执行。

委派时 policy 向上、mechanism 向下：Info 只写目的、承诺、不承诺、不变量、验收和预算，不指定内部实现方案、数据结构或命令序列。上层同时规定语义和实现会一并消灭下层的抽象空间和本层的认知节省。

### 认知过载是边界信号，不是努力信号

出现下列任一情况时，正确动作不是“再想一想继续做”，而是停止向下钻，先把当前未完成范围契约化成可独立验收的单元并一次 `coordination_requestHelp`：

- 开始逐例硬编码，或把同一规则手工重复展开；
- 上下文里同时维持多个互不相关的知识域；
- 已推进相当篇幅而仍无契约转为 DONE。

“未获帮助则继续”只适用于 manager 实际拒绝之后，不是跳过请求的理由。

### 判据是思维方式，不是可清点的指标

第一版把「必须委派」写成了可枚举的触发条件（多个独立内部决策 / 逐单元套用同一规则 / 大量顺序试错）。实测里 planner 立刻把它当成了要核实的指标：`prompt-v9` 那次 run，planner 连发 60 次 bash、写了 2 个近似脚本反复清点源文件分组，上下文涨到 137K token——它在替 executor 做证据工作，而不是建模。

所以判据改成一个问题而不是一组数字：**把内部装进本层之后，本层还剩多少判断力？** 配套在 planner 的调查规则里写死停止条件：

> 调查在计划不再改变时停止，不在数字被核实时停止：规模由已知结构推断，同类事实确认一次即止，不写脚本反复清点，清点是 executor 的证据工作。

判断力是想出来的，不是数出来的；一旦判据可以被清点，模型就会去清点它。

### 认出形态，而不是通过考核

把判据改成「内部是否自成一个要单独理解的领域」之后，`prompt-v9` 仍然是 0 次拆分——门槛听起来像在要求一个子系统，于是没有东西够格。所以门槛重述成**能否独立说清并独立验收，不是它有多难或有多大**，并把常见形态直接列出来给模型认：

| 形态 | 长什么样 | `admission_reason` |
| --- | --- | --- |
| 互不相交的写入面 | 同一累计契约下两组写入面不相交，各自能用自己的命令判对错——同在一个文件也算，每组都不大也算 | `critical_path` |
| 冻结接口后的内部选择 | 接口、用法、验收已冻结，只剩一堆只影响内部、答案不改变父级决定的选择 | `context_offload` |
| 说不清谁对的候选 | 同一问题两个候选，同一 gate，最多采纳一个 | `race` |

这份形态表放在 `coordination_requestHelp` 的 description 里，而不是 system prompt 里：它只对真正持有该工具的角色可见，也不占系统提示词预算。planner 侧则以`可拆例`、`卸载例`两条短例承载同样的识别方式。

判据要让模型**认出**合适的情况，而不是让它**证明**自己够格；后者的稳定解永远是不拆。

### 让候选先存在，而不是等它被逼出来

`prompt-v10` 还是 0 次拆分：planner 108 次 bash，executor 228 次 bash，峰值 180K token。观察到的行为是——agent 在挑战自己的思考极限，想找到自己做不到的那一刻再分派。

这是提示词写出来的。当时 `什么时候请求帮助` 的四条触发里，有一条是：

> 你开始逐例硬编码、手工重复套用同一条规则，或同时维持多个互不相关的知识域，或推进相当篇幅仍无契约转 DONE。

这是**滞后指标**：它要求失败先发生。字面读下来就是「撑到开始退化再拆」。配上「分派是副产品，不是目标」「未获帮助则继续」，以及输出模板里 `split: none` 排在第一位，整条链的稳定解就是永远不拆。

更根本的问题是：**没有任何一步会产生候选单元**。前三条形态都写成「认出来就提交」，可模型手里从来没有一份候选清单可认，于是每个 gate 都对着空集求值，结果恒为不拆。

改法是把分组变成一个**固定发生的常规动作**，位置在契约写完之后 / 首次写入之前，对象是**任务本身的形状**，而不是 agent 的疲劳程度：

> 写完契约后固定做一次分组，这是常规动作，不是例外：把每条契约映射到它要改的写入面（文件、符号、状态），写入面互不相交的契约分到不同组。分组的结果是候选单元。接下来才由认知闭包决定哪些候选要合并回来——先有候选，再做取舍。

流程因此反了过来：不再是「证明你需要帮助」，而是「这些是单元，哪些必须合并」。`split: none` 从默认起点变成分组只剩一组时的结果，输出模板里也改成先列分组、再写结论。

滞后信号保留，但降级成「你漏了那一步」而不是触发条件：

> 如果你已经在逐例硬编码……说明开工前那次分组漏了。立刻补做，不要再撑一会儿。

同时补上动机层的反框架——模型把委派当成能力不足的自认，所以宁可硬撑：

> 委派不是承认做不到，也不是走投无路才用的退路。上层留住的判断力是稀缺资源，把已经闭合的边界交出去正是为了保住它。

### 分组按写入面切，会只切下叶子

`prompt-v12` 第一次真的拆了：executor 提交了一次 requestHelp，manager 准入三个 `context_offload` helper——`cmake/Detect.cmake`、`cmake/Sources.cmake`、`cmake/Version.cmake` 加两个模板。但 manager 自己的话说明了剩下发生了什么：

> C4（顶层 CMakeLists.txt 合流）、C5、C6、C7 及门禁 G1–G13 按你的计划由 executor 在 Wave 2 自行承担，未另行创建节点。

于是 task-1 的 executor 独自做了 28 次 edit、274 次 bash，跑了 2h39m、172K token，然后 failed；续接的 task-5 同样没拆，1h17m 后再次 failed。拆下去的是三个已经冻结的叶子数据文件，留下来的是全部集成工作。

三个原因，都在提示词里：

1. **分组判据是写入面，而这个任务里几乎所有东西都落在同一个文件上。** 写入面不相交这一条正确地分出了三个 `cmake/*.cmake`，然后把其余全部塌进一组——判据本身偏向切叶子、留集成。现在补上：大批契约挤在同一个文件上，是接缝还没切出来，不是这件事不可分；先按各自编码的决定分开，再为每组切出自己的写入面（独立的被包含单元、独立的 target、独立的区段），顶层文件只做引用和合流。
2. **`唯一 integration owner` 被读成了「由我写完全部内容」。** 它的意思是由谁合流和跑最终门禁。已在 executor 侧写明。
3. **Wave 2 被声明出来，然后被默默吸收。** 「只展开当前 frontier，后续写激活条件」之后，没有任何一步要求在条件满足时重新判断，executor 就直接接着做了。现在要求：每个 wave 的 helper join 回来之后、进入下一段工作前重做分组；后续 wave 不因为“已经排在我的计划里”就归自己做完。

这一轮的教训是：**拆分率从 0 变成非 0 之后，要看的是拆下去的是哪一部分。** 只offload 已经冻结的叶子，是把最容易的部分交出去，留住最需要卸载的那部分。

### 拆分不该是 planner 的一个决定，它就是 planner 的工作

前面几轮一直在调「什么时候该拆」，说明这个问题被放错了位置：拆分被当成 planner 众多职责里的一个 corner case，要靠触发条件、门槛和信号去唤起。

planner 的职责其实只有一件事：**把需求表达成一层抽象——这一层承诺什么、不承诺什么、为兑现承诺需要下层提供哪些能力，以及每样能力凭什么算数。** 这正是原讨论里那四个问题。而其中第三问的答案，就是子单元清单：

> 为兑现上面的承诺，列出这一层需要的能力，每样用它的语义命名，不用实现命名。这一步不是“要不要拆分”，它就是这一层抽象的内容——能力清单即子单元清单。

于是没有「要不要拆」这个决定需要做。能力存在，是因为这一层需要它，不是因为通过了某个门槛。planner 的分节据此重排成一层抽象被定义出来的顺序：

`这一层承诺什么` → `这一层不承诺什么` → `需要下层提供哪些能力` → `哪些决策归我，哪些下放` → `哪些能力现在就能交出去` → `每样能力凭什么算数`

配套的另一半是：**这一层的实现要分派出去**，这才合上原讨论的结论。

> 归我的是这一层的定义：承诺、不承诺、能力划分、依赖方向和验收。下放的是这些能力的实现。定义完成之后，默认由下层实现，本层负责合流与最终门禁——本层把实现留在手里，只在它是本层概念的自然写法时才成立。

executor 侧对应改成：「你是这一层的 integration owner：默认由下层实现 planner 列出的各样能力，你负责合流和最终门禁，只有本层概念的自然写法才自己写。」这直接针对 v12——当时 executor 把「唯一 integration owner」读成了「由我写完全部内容」，于是把三个叶子文件交出去、自己扛下全部集成。默认方向反过来之后，留在手里的才需要理由。

原来的 `按写入面分组` 一节因此降级：它不再负责产生单元（单元来自能力清单），只负责检查这些单元的写入面是否冲突。这也去掉了它结构性偏向切叶子的毛病——写入面判据会把所有最终写进同一个文件的东西塌成一块，而能力判据不会。

## 5b. 记忆图：状态、冲突与证据失效

`prompt-v12` 的记忆图（78 个节点、75 份不同正文）在连续性和关键事实召回上表现良好，但三项判断能力接近于零，原因都在提示词：

| 症状 | 证据 | 根因 |
| --- | --- | --- |
| 状态字段是死的 | 78/78 节点 `status: accepted`，其中 7 个 `kind: hypothesis` | 提示词只列了合法取值，没说什么时候写哪个；`accepted` 成了默认 |
| 冲突不被提出 | 图里同时有「用户传入 CMAKE_C_FLAGS 仍须保留优化链」和「传 flag 后无 -O3 是正确的」，两条都是 accepted | compact 被明确要求「不改旧状态，冲突来源保留」，却没有任何一句让它**标记**冲突 |
| 证据无法复用 | task-5 继承了大量旧证据仍重跑全矩阵 | 节点只记「做过什么」，不记「什么改动会让它失效」 |

对应的三处改动：

- **状态按证据定。** `accepted` 只给有证据锚的 fact 和来源明确的 directive；hypothesis 一律 `disputed` 并写清什么证据会证实或证伪。`memory_apply` 的 description 里补了四个状态的语义——「accepted 不是默认值，把 hypothesis 留成 accepted 等于向下游谎报确定性」。v12 里 manager 就是拿一条 accepted 的 VFS 删除方式猜想直接生成了 task-6。
- **冲突由新节点携带。** compact 改不了旧节点，所以它必须在写入前拿主体/条件/范围去对已有节点，冲突时新节点写 `disputed` 并点名冲突 ID 和两边证据强度——「不写就等于把它藏了」。与 directive 冲突的观察额外标注为缺陷信号，而不是一条新事实。deep organize 是唯一能改旧状态的地方，因此冲突裁决明确指派给它。
- **证据写清失效条件。** 每条证据 fact 要写验证对象、证据成立时该对象的锚，以及什么改动会使它失效。只写「跑过什么、结果如何」的证据下游判断不了它是否仍然成立，只能重跑。

成本侧（13 次 compact + 1 次 organizer ≈ 25.5 分钟、305K token；单次深度整理 21 候选选中 3 个却烧掉 139,748 token）改的是展开纪律：严格 1 → 2 → 3，只对真正要判断的少数节点取全文，其余停在级别 2，看完立即 collapse；展开成本要和改动量相称。

**去重那条改不了提示词。** 4 份共 47,283 字符的重复用户消息是 [`internal/coordination/stores.go`](../internal/coordination/stores.go) 按 taskID 生成的 `task-user-input-<taskID>`，且是 protected 节点，提示词既写不了也删不掉。要修得在代码侧按内容去重或让续接 root 引用首份。

## 5c. 默认必须写在判据里，问题必须问反方向

把 planner 重排成「定义一层抽象」之后，还剩三处断点会让它照样不分派：

1. **默认和判据分居两节。** 「默认由下层实现」写在 `哪些决策归我，哪些下放`，而真正执行判断的下一节开头是「选择继续精化、委派或直接实现/复用」——一个中立的三选一。模型执行的是判据那一节，默认那句不在它眼前；把每样能力都路由到「直接实现/复用」完全合规。默认已移进判据本身。
2. **提问方向反了。** 节名原为 `哪些能力现在就能交出去`，等于要求为每一次交出举证。默认既然是下层实现，该举证的就是留下。现在叫 `哪些能力留给自己写`，开头写死：「默认每样能力都由下层实现。下面的三问是用来找出例外的，不是逐个批准交出去——需要理由的是留下，不是给出去。」留下只有两种合法情况（本层概念的自然写法；做它必然要回头改本层判断），且必须在计划里写明是哪一种。
3. **输出不强制逐能力归属。** 原来只要求「列能力清单及各自写入面」，清单可以整体滑给 executor。现在每样能力必须标 `下层实现` 或 `本层自己写`，后者写明属于哪一种例外；标为 `下层实现` 的就是 executor 要提交的 helper 单元。executor 侧对应：「planner 的能力清单里标为 `下层实现` 的，你不自己写，一次 requestHelp 把它们提交出去。」

一个反复出现的规律：**只要判据是中立的三选一，稳定解就是全选「自己做」。** 想让某个分支成为默认，那句话必须出现在判据本身的开头，而不是相邻的一节里；并且问题要问需要理由的那一侧。

### 机制要点名，数量要背书

把 planner 的语言抽象化之后，冒出两个新窟窿：

**「下层」这个词读不出 helper。** `下层实现` 在 planner 里出现四次，却从没有一句说「下层」指的是另一批 agent、经 help 协议物化；唯一的连接放在输出格式的末尾。而在一份满是抽象层语言的提示词里，「下层」最自然的读法是**代码的下一层**——同一个 executor 自己写。机制现在写进开篇第一段：

> 这里的“下层”不是代码的下一层，是另一批 agent：你列出的能力由 helper 并行完成，executor 一次 coordination_requestHelp 提交，manager 物化成 helper 节点，做完 join 回 executor 合流。

**没有任何一句为「多」背书。** 提示词里关于数量的话全是抑制方向：「并行度是正确抽象的副产品，不是目标」「不凑宽度」。这些原本是防伪造单元的，但叠在一起就成了「少即安全」。现在补上正向表述，并给「副产品」加了限定：

> 切到每样能力都能被独立说清和独立验收为止。切得细是正常的，一个 wave 十几个互不相交的单元并不奇怪。清单短才需要解释：两三样能力覆盖一个整仓库级交付，通常说明还停在复述目标，没有真正拆开。

> 扩展性和并行度是正确抽象的副产品，不是目标；副产品不等于少——抽象切对了，单元自然会多。

真正的护栏没有松：每个单元仍必须能独立说清、独立验收、写入面互不相交。被鼓励的是「切到每样都能独立验收为止」，数量是这件事的结果，不是指标。

### 结构上照 Codex harness 的写法

Codex 对「什么时候用 `update_plan`」这类自由裁量决定的写法，正好是这个问题的成熟解，顺序是：

1. **它是什么、为什么有用**（一段正面说明）；
2. **一句反滥用护栏**——只有一句，不是一张禁令表（“plans are not for padding out simple work with filler steps”）；
3. **`Use a plan when:` 正面触发清单**——写成可辨认的处境，不是要证明的门槛；
4. **成对的正反例**：同样三个任务，先给三个 high-quality plan，再给三个 low-quality plan，让对比本身承担教学；
5. **一句收束**：“If you need to write a plan, only write high quality plans, not low quality ones.”

正面引导与禁令的比例大约是 20:1，且没有任何阈值。`coordination_requestHelp` 的 description 现在照这个顺序重写：意图与暂停语义 → `在下列情况请求帮助` 四条处境 → 一段门槛与反滥用 → 同一任务的`好的帮助单元`/`差的帮助单元`对照 → 一句收束 → `字段与协议`。机械字段挪到最后，因为它们回答的是“怎么填”，不是“要不要拆”。

放在工具 description 而不是 system prompt 里也是同一个道理：它只对真正持有该工具的角色可见，且不占系统提示词预算。planner 的`可拆例`、`卸载例`同样改成了自带反例的对照句。

这一条是从实测轨迹加回来的。旧提示词用 `width_class != none` 做机械触发，强制 executor 在首次写入前提交 ready frontier；语义精化改写把它换成认知闭包后，只保留了“不闭合就不许委派”这一半，删掉了“闭合就必须委派”那一半。在同一个 libsodium Autotools→CMake 迁移任务（119 个翻译单元、~40 项探测、67 个公共头、80 个测试程序）上的对照：

| 提示词版本 | `[拆分请求]` 次数 | 单 executor 峰值上下文 |
| --- | --- | --- |
| 机械触发（08-27） | 1–4 | ~96K–107K token |
| 认知闭包首版（08-28） | 0 | 108K–187K token，一次跑到 1h25m / 6.4M token 仍未收敛 |

零次拆分不是纪律，是委派失败：单个 executor 连续发出 129 次 bash 调用逐例套用同一条规则，正是讨论里“超出语义工作集之后退化成硬编码”的形态。所以判据必须双向，并且要有过载探测把它触发出来。

工具仍需要机械上完整的帮助单元：

- `admission_reason`；
- 目标；
- 已知输入；
- 不可违反的约束；
- 独立交付物；
- 写入面；
- evidence recipe；可执行行为写命令、预期观察与退出码，结论/文档可用可定位来源或解析结果；
- 依赖、输出格式、明确不做什么、阻塞与返回条件。

单元 ready 后，`admission_reason` 只允许：

1. `critical_path`：它能与另一个 ready 单元并行，从而缩短依赖链。
2. `context_offload`：接口、使用方式和验收方式已冻结；列出被隔离的内部问题，且答案不改变父级决策。
3. `race`：`race_basis` 取 `user_requested` 或 `unresolved_alternatives`；后者给出至少两个不同候选、同一 gate、唯一 adjudicator 和不能先用更小实验消歧的原因。

短例：

- 同一知识、同一变化原因的决定留在一起，与文件数量无关。
- 还说不清“正确”就继续精化；会用、会验且内部不影响上层决策后，简单叶子直接做，复杂域必须委派——“继续自己做完”不是这一格的合法选项。
- 同一规则要在上百个单元上逐个应用，或一个交付要同时维持多个互不相关的知识域时，先契约化再分派；压进一个 owner 顺序完成是委派失败，不是纪律。
- 两种方案只有在回答同一问题、使用同一验收且最多采纳一个时才 race。

计划必须同时满足：

- I1 覆盖唯一：每条累计契约只有一个最终 acceptance/integration owner；race candidates 只拥有候选产物。
- I2 写入隔离：同 wave 的互补单元写入交集为 0；race 豁免，在隔离工作区可重叠写入/验收，只有采纳与合流经过唯一裁决点。
- I3 独立可判定：已声明依赖物化后，integration owner 或 helper 自带 verifier 不读取未声明兄弟结果即可判定。
- DAG 无环，ready frontier 精确，所有分支只在一个 integration owner 合流。

关系只保留四种：独立交付 `section`、真实数据依赖 `pipeline`、互斥根因 `hypothesis`、同契约隔离候选 `race`。race_basis 取 `user_requested` 或 `unresolved_alternatives`；后者记录至少两个不同候选、同一 gate、唯一 adjudicator 和不能先用更小实验消歧的原因。最多采纳一个，依赖边不能掩盖写入冲突。

Join 按路径应用，不按符号应用。同一路径的互补候选必须先 inspect/compare，再由 integration owner 在自身工作区人工合成；不得用 `replace` 覆盖先前结果，人工吸收后带理由 discard 来源，再 finish 并跑组合门禁。

### 计划要 decision-complete，接口是缝

executor 反复自己做大量调查，是因为计划只给了目标和验收，没给**接口**。收到「把 A 落到 dir1/，验收 `cmd1`」的 agent 并不知道 dir1/ 该导出什么名字、上层怎么消费，只能自己再设计一遍——这既是重复调查的来源，也是不拆的来源：**接口没写死，缝就不存在；缝不存在，就只能由一个 owner 同时拿住两边。**

调研了三份参考实现，命中的是 deepseek-harness 的 plan-mode section（`apps/cli/config/agent-presets/*/agent.cordis.yml`）：

> Make the plan decision-complete: state the goal and success criteria; group implementation changes by subsystem; **identify public API, schema, and data-flow changes**; cover edge cases, failure modes, tests, acceptance criteria, and explicit assumptions. Keep it concise enough to review but **detailed enough that another engineer can implement it without making design decisions**.

我们已经有 “decision-complete” 这个词，缺的是它后面那份内容清单——尤其是 public API / schema / data-flow。Codex 的正反例给的是同一件事的另一面：high-quality plan 是「Parse Markdown via CommonMark library」这种落到具体制品上的动词，low-quality 是「Create CLI tool」这种复述目标。

两处改动：

- 新增 `## 把接口写死`：每样能力都要带具体接口，用真名不用描述——导出什么（文件路径、符号、变量、目标名、schema 字段）、消费什么、跨边界不变量。并把它接到设计完成度和调查下限上：「写不出接口，说明这一层还没设计完」「这也是调查的下限：查到每样能力的接口写得出来为止」。
- `## 输出` 开头补上 decision-complete 的内容清单，加一条分界线——**写死设计，不写实现**：接口、数据流、验收、边界情况具体到真名；算法、命令序列、内部结构留给 owner。附一对计划项正反例。

`哪些决策归我，哪些下放` 并入 `需要下层提供哪些能力`（两节都在讲所有权划分），腾出的字节给了新内容。

### 写死接口 ≠ 枚举实例

上面那条改完就过头了。`fw01-deepseek-planner-help-v2` 里，planner 峰值 **161,582 token**（30 bash + 17 read + 6 ls），逐项确认路由、HTTP 方法顺序、Cookie、错误页、Swagger 模板、依赖元数据。它没有堆抽象层，问题是**抽象边界迟迟不收敛，于是不断向具体实例下钻**——把「设计一层抽象」做成了一张行为清单。

最关键的一个数字：同一次 run 里 `task-2:executor` 仍然跑了 **95 次 bash、峰值 205,073 token**。planner 那 161K 的实例级细节**没有减少 executor 的调查**。两边在查同一片地，代价付了两次。这说明枚举实例根本不是 executor 需要的东西：它需要的是形状（据此本地推出实例），以及它自己的局部事实（planner 本来就替不了）。

问题出在「接口具体到真名」和「写死设计」这两句没有区分**形状**和**取值**。补上的分界：

> 写死的是形状和不变量，不是取值：一条规则覆盖一类，不逐个列出这一类的成员。写「路由表由 X 导出，形如 (方法, 路径, handler)」，不写两百条路由。写到第三个同型条目时，该写的已经是那条规则——再列下去是替 owner 先做它的局部事实，既撑爆本层上下文，又让 owner 没有可决定的东西。好边界看的是一个概念封装了多少变化。

同时给调查补上**上限**——上一轮只给了下限，这正是失衡的来源：

> 调查因此有下限也有上限：下限是接口写得出来，上限是你开始逐个确认同型实例——到那时接口已经够了，停下来输出计划。

计划项正反例也改成双向：「实现 A 模块」太粗不可用，「逐条列出 `Foo` 的每个入参取值」太细同样不可用，因为它把 owner 的活先干了。

一个反复出现的规律：**每次只给一个方向的判据，模型就会走到那个方向的极端。** 下限要配上限，"不够细"的修正要同时说明什么算过细。

### executor 凭什么信任计划

计划写细了，还得允许 executor 相信它。原来不允许——两条指令在推它重查，而且互相冲突：

- `授权与可信输入`：「其他材料须由当前工作区核对」——计划就是“其他材料”。
- `开工前`：「再用文件、定义和命令核对计划假设」——无界，没说核对到哪算完。

已有一句缓冲（「若现有计划已明确目标、owner、接口和门禁，就直接执行」），但它只禁止**重新规划**，不禁止**重新调查**；而那条具体的「核对计划假设」更可操作，所以它赢。这解释了为什么 planner 烧掉 161K 之后 executor 仍要跑 95 次 bash。

缺的是**信任边界**——计划里哪些是设计、哪些是观察：

> 计划里的设计直接采纳：owner 划分、接口、验收口径是 planner 的产出，不是待核对的观察，重新推导等于把设计做第二遍。计划里的观察（路径、符号、当前行为）只在你要依赖它写入时核对，且只核对你要动的那部分。计划的 `未知项` 就是留给你验证的清单——不在清单上、又不挡这次写入的，不再查一遍。

这条同时接上了 planner 输出契约的第四段。`未知项` 本来就是设计好的交接口——“仅列仍需 executor 验证的假设、替代路径和未覆盖面”——但 executor 侧从来没有指向它，于是它形同虚设。`证据与门禁` 也改成先用计划给的 evidence recipe，计划没给时才按契约类型自取。

冲突的那条全局规则也收窄成「除计划的设计部分外，其他材料须由当前工作区核对」——上游报告、继承记忆仍然要核对，只有本 root 计划的设计部分例外。

### 模型会用我们的判据来论证不拆

`prompt-v14` 的 rollback run 留下了迄今最有价值的一份证据：planner 烧掉 **152,887 token**、66 次调查，产出 **15,245 字符**的计划，结论是 **`split: none`**。executor 的第四条消息是「No ready frontier per plan (split: none), so I proceed directly with implementation」，随后自己又跑了 39 次 bash。

计划本身正是「过细的行为清单」：精确到字节数（40535B、189B、195B、2387B）、cookie 序列化字面量、digest challenge 的参数顺序、62 条路由规则、完整依赖闭包。但决定性的不是它多细，而是它**拒绝拆分的论证方式**——它用的全是我们自己的判据：

> 贯穿全部交付的同一个不可分语义决定是——「httpbin 完整可观察 HTTP 行为在原生 ASGI 上的字节级保持」。……oracle diff 是全部子片的同一个验收，任何子 owner 都无法独立验收。

> 尝试拆分（如 helpers/core、core/spec、app/tests、app/packaging）会产生……跨 owner 契约（冻结路由表、冻结 request-context 接口、冻结 import 面）。

三个漏洞，都是判据本身留的：

1. **「不可分的语义决定」可以用任务目标复述来满足。** 「把 X 完整迁到 Y 且行为不变」对任何任务都成立——这个逃逸口是通用的。补：那个决定必须是设计秘密，换个说法就等于 Task Info 目标的句子不构成不可分。
2. **「必须能独立验收」被读成「不能共享最终 oracle」。** 而它自己的计划给每个 wave 都写了独立证据 recipe。补：共享的最终门禁不使子单元无法独立验收，子单元的验收是同一 oracle 的子集；能写出证据 recipe 就能独立验收。
3. **它点名了正确的缝，然后把冻结这些缝当成成本。** 冻结路由表、request-context 接口、import 面——那正是 `把接口写死` 要求 planner 做的事。补：若拆开需要先冻结接口，那是本层该做的设计，不是不拆的理由。

这一轮的教训比前几轮更普遍：**每加一条判据，就等于给模型多一个论证不拆的素材。** 判据是双刃的——写「必须能独立验收」时要同时写清什么不算「不能独立验收」，否则它会被当成不拆的许可证。

### 钟摆甩到另一端：72 个 task、6 层深

`prompt-v15` 的计划索引记录了相反的极端：**72 个 task，40 failed / 32 active，最深 6 层**，五个 root（`1 → 9 → 12 → 17 → 41`）反复重新分解同一个需求。六个问题：

1. **递归下钻成无实现内容的只读审计。** task-33–40、67–72 的交付物是「只读核对」「只读审计」「只读盘点」。最极端的是 `62 → 69 → 71/72`：brotli 实现 → 审计 brotlicffi 是否存在 → 动态 probe import + 静态审计 import。这些是**调查**，不是能力。
2. **粒度小于可独立验收的实现边界。** 索引自己的观察 3 说得很准：「产物之间高度依赖，计划粒度明显小于可独立验收的实现边界」。task-39/40「再单独核对……」的产出只是另一个单元的输入。
3. **同一能力被五个 root 反复重新分解。** 请求解析归一化在 task-2 / 13 / 23 / 43 各出现一次。修复 root 从头再规划，而不是增量。
4. **`race_basis=user_requested` 被误标。** gzip/deflate/brotli 是三个都要交付的互补能力，被写成候选竞速；用户从未提出多候选要求。
5. **harness 故障被写进项目交付。** task-41 把 Threadmill 发布器的跨设备 rename 故障写成 httpbin 仓库内的修复要求，而被评 agent 根本改不了发布器。
6. **递归没有终止条件。** 深度只由模型想不出更小的切法来终止。

根因只有一个：**helper 的交付物类型从未被约束。** 只要「审计某依赖是否存在」也算一个可独立验收的单元，递归就没有底。补的规则：

> helper 必须形成新的所有权边界，交付能直接对最终 oracle 判定的结果。实现任务中，只给别的 owner 当输入的核对、盘点或 probe 仍归该 owner；结论、研究或评测任务中，正交证据面本身可以独立交付。普通步骤和父任务的重复核验不包装成 helper。
>
> 拆到能独立对着真实验收判对错为止。再往下切出的单元若产物只是另一个单元的输入、无法用最终 oracle 的子集判定，就是切过头，退回上一层。

race 那条也补了反例：「gzip/deflate/brotli 这类都要交付的互补能力不是候选，`user_requested` 更不能由你推断」。

**还没修的**：问题 3（修复 root 重复分解）和问题 5（harness 故障写进项目任务）分别属于 planner 的 `规划修复` 与 manager 的报告审计，需要单独一轮。

另外值得记一笔：planner 提示词此时已达 **11KB**，是第二大提示词的两倍多。一份满是分解理论的提示词产出 72 个 task 六层深，很难说这两件事无关。下一步该做的可能不是再加规则，而是给 planner 减重。



## 6. 角色契约

每个角色的提示词末尾都带一组`短例`，覆盖该角色最容易做错的那个判断：planner 是边界/闭包/可拆/卸载/候选，executor 是该拆与该写 DONE，manager 是准入与收尾，verifier 是门禁与判定，organizer 是相关性与状态。

| 角色 | 方法 | 可验证成功标准 | 固定交付 |
| --- | --- | --- | --- |
| Manager | 将普通消息、`[拆分请求]`、task 报告分路；保真建 root；按逐契约直接证据审计报告；机械故障只追加能改变失败输入或运行状态的 root | Task Info 的用户硬要求遗漏 0、无来源约束 0；普通消息不调用 provideHelp；未验证契约为 0 才接受 PASS；重复 continuation root 为 0 | 给用户的简短状态或最终结果 |
| Planner | 从目标递归做语义精化；沿知识边界分配 owner；用认知闭包决定继续精化、委派或直接实现/复用，闭合而下层庞大时委派是义务 | 契约覆盖 100%；只有语义变化穿过边界；executor 无需再做架构选择 | 完成定义、执行图/Help Executor 计划、集成门禁、未知项 |
| Executor | 写前复核未决节点的认知闭包，不服从 `split:none` 标签；闭合而实现域庞大时必须整批请求 ready frontier，过载即停止向下钻；维护契约证据账本并在最后改动后复验 | 每条契约标 DONE/UNVERIFIED/BLOCKED，DONE 有直接证据；等价重复门禁 0；所有 join 来源已处置并 finish；范围外改动 0 | 最终工作区与执行报告 |
| Verifier | 首次工具前建立有限验收表；每契约执行最强直接门禁；只补一个可观察反例门禁；证据覆盖后停止；只用 executor 基线裁定 | 每条硬契约显式映射到 PASS/FAIL/UNVERIFIED 与直接证据；PASS 时 UNVERIFIED=0；等价复验 0；候选实现不能把 FAIL 变 PASS | PASS / FAIL / INCONCLUSIVE 报告 |
| Compact | 只抽取可见输入；按主体/条件/结果/范围做语义去重；按证据区分 directive/fact/hypothesis；只写差量并剔除秘密 | JSON 可解析；秘密泄漏 0；fact 有证据锚；语义重复节点减少；未决失败、精确 ID、下一步不丢 | 记忆节点 JSON |
| Organizer | query 渐进召回最小视图；deep audit 只审输入列出的有界节点；保留不确定冲突 | 无关召回 0；实际 ID 100%；保护节点写入 0；每项变更有证据和 reason；全局整洁不是目标 | 模式、选择数、变更数、理由 |

Planner 与 Verifier 的工作区是一次性的；Planner 只保留计划，Verifier 只保留报告。Executor 的工作区持久。三种任务角色都可请求 help，但交付随角色变化：Planner 收规划证据/候选，Executor 收实现，Verifier 收验收证据；Manager 只响应真实请求并守住无环来源。Verifier 发现缺陷仍只报告，由 Manager 决定是否追加 task。这些结果来自环境和协调机制，不靠硬编码保存列表实现。

当前 compact 运行时把整理模型的输入限制为 16 KiB，中段可能省略；被裁剪前缀随后仍会从近期历史移除。新提示词只要求如实记录可见缺口，不能恢复模型未见内容。分块、可确认的原子 compact 是独立运行时改进，不属于本轮文案优化。

## 7. 提示词验收

### 静态

- 每个完整 prompt 的四段存在且顺序固定。
- 角色只提自己拥有的工具和决策。
- 未定义协议、固定并发档位和重复方法段为 0。
- 真实项目配置继承内置 prompt，不保存旧副本。

测试中的 25,000 UTF-8 bytes 是防止本轮精简反弹的仓库回归预算，不是 tokenizer token 上限；真实 token、cache 和延迟另按下述性能指标采集。

### 轨迹

- 请求分类正确率、越权改动数。
- 累计契约覆盖率、重复 owner 数、same-wave 写入冲突数。
- helper 接受率、重复劳动率、join 冲突率、repair root 数。
- 各角色模型/工具轮次、等价重复门禁数、逐项直接证据覆盖率。
- verifier false PASS / false FAIL / INCONCLUSIVE 率、UNVERIFIED 数和覆盖后继续调用数。
- compact 关键状态保留率、语义重复率、无锚 fact 数、秘密泄漏数；organizer 相关召回率、无关召回数和 token。

### 性能

- 各静态 prompt 的 bytes/tokens。
- prompt cache read/hit、动态前缀变化次数。
- 各角色输入/输出 token、TTFT、总耗时。
- helper 带来的关键路径变化与额外 token。

同一批代表性任务做 A/B，每次只改一组规则。保留改动的停止条件是任务成功与证据完整性不下降，并且 token/延迟代价符合目标；“感觉计划更好”不是验收证据。

## 8. 每份提示词的来源映射

“参考”表示迁移了可验证方法，不表示逐字复制。Threadmill 的协调、VFS、Join 和记忆语义均以本仓库运行时为准。

| 模型输入 | 位置 | 方法来源 | Threadmill 专有部分 |
| --- | --- | --- | --- |
| 可配置通用 prompt | `prompts.default` | OpenAI 的 outcome/constraints/evidence/success/output；Pi 的极简提示 | 任务类型、task 继承基线、join 完结、授权边界 |
| 无配置 system fallback | `loop.go:DefaultSystemPrompt` | OpenAI 的最小 outcome/evidence/stop contract | 授权、项目路径、继承基线；不含 Join 协议 |
| 记忆压缩 | `prompts.compact` | Pi compaction 的 goal/constraints/progress/decisions/next steps 与精确路径/错误 | directive/fact/hypothesis、证据锚、16 KiB omitted 缺口、旧节点只能由 organizer 更新 |
| JSON 修复 | `prompts.compact_json_reminder` | OpenAI 的结构化输出修复原则 | 本地 memory schema |
| 可配置上下文压力 | `prompts.drop_context_pressure` | Pi 的 checkpoint/critical-context 保留 | 有对应工具时调用 `memory_drop_from_context`，以 `rewritten_messages` 验证 |
| 压力提醒 fallback | `drop_context.go:dropContextPressureReminder` | Pi 的 critical-context 保留 | 无工具手册；保留目标、硬约束、未决证据和下一步 |
| 记忆查询控制 | `prompts.organize_query` | Pi 的最小相关上下文；DeepSeek Harness 的静态/动态分段 | 只用实际 node/target ID、最小必要视图 |
| 普通整理动态包装 | `factory.go:organizeQuery` | 无外部文案移植 | 当前 query、target、子图目录和候选节点 |
| 工具说明 | `threadmill.yaml:tools.*.description`；`internal/tool/*.go`、`internal/coordination/{graph_tools,help,join_tool}.go`、`internal/agent/{factory,hidden_tools,drop_context,memory_view}.go` 的 schema/fallback | Pi/DeepSeek Harness 的按实际能力注入 | 参数、副作用、错误恢复、Join 和 help 准入协议 |
| Manager | `agents.manager.system_prompt` | OpenAI manager-as-tools；Anthropic orchestrator-workers | 三路输入、root 串行/helper 并行、Task Info 保真、增量 repair root |
| Planner | `agents.planner.system_prompt` | 用户提供讨论中的语义精化/知识边界/认知闭包；DeepSeek Harness decision-complete plan；Eino 委派判定；Anthropic 委派契约 | IPD、I1/I2/I3、四种关系、当前 frontier、一次性规划工作区 |
| Executor | `agents.executor.system_prompt` | OpenAI change/build 的最终状态与证据；DeepSeek worker completion 不是 certification | 唯一 help 请求者、持久 VFS、join 路径级人工合成 |
| Verifier | `agents.verifier.system_prompt` | OpenAI evidence/success contract；Anthropic evaluator-optimizer 的明确评价标准 | G/C 必要性与充分性、一次性 VFS、只裁定 executor 基线 |
| Organizer | `agents.subgraph_organizer.system_prompt` | Pi 的相关上下文最小化；其余为 Threadmill 原生 | query/deep 工具分流、可整理的混乱、保护层、证据仲裁 |
| 深度整理动态请求 | `curation.go:deepCurationQuery` | 无外部文案移植 | 明示只审核实际提供的有界节点，不声称全图 |

## 9. 一手参考

- [Codex harness system prompt](https://github.com/openai/codex)：自由裁量决定的写法——正面触发清单 + 同一任务的正反例对照 + 一句收束，禁令只占一句。
- [用户提供的“软件工程可扩展性”讨论](https://chatgpt.com/share/6a904448-6700-83e8-9367-9e46da73470f)：语义精化、知识边界、认知闭包、上层语义/下层机制，以及扩展性作为正确抽象的副产品。
- [OpenAI Model guidance](https://developers.openai.com/api/docs/guides/latest-model)：精简提示、每条规则一次、目标/约束/证据/成功标准/交付格式、在代表性任务上比较。
- [OpenAI multi-agent patterns](https://openai.github.io/openai-agents-python/multi_agent/)：manager-as-tools 保留综合与最终输出责任。
- [Pi system prompt builder](https://github.com/earendil-works/pi/blob/a470b12/packages/coding-agent/src/core/system-prompt.ts#L79-L169)：仅注入实际能力的规则，稳定与动态上下文分层。
- [Pi compaction](https://github.com/earendil-works/pi/blob/a470b12/packages/coding-agent/src/core/compaction/compaction.ts#L467-L535)：保留 goal、constraints、progress、decisions、next steps 和精确路径/错误。
- [DeepSeek Harness system prompt](https://github.com/deepseek-ai/deepseek-harness/blob/99f6f02/packages/core/system-prompt/src/index.ts#L128-L239)：有所有者、有顺序的 prompt sections 与动态上下文。
- [DeepSeek Harness plan mode](https://github.com/deepseek-ai/deepseek-harness/blob/99f6f02/apps/cli/config/agent-presets/standard/agent.cordis.yml#L113-L124)：decision-complete plan；Threadmill 不照搬其只读限制。
- [DeepSeek Harness team task contracts](https://github.com/deepseek-ai/deepseek-harness/blob/99f6f02/packages/experimental/tool-agent-team/src/index.ts#L173-L195)：目标、依赖、写入面与 acceptance；Threadmill 另加 VFS/Join 约束。
- [DeepSeek Harness worker completion](https://github.com/deepseek-ai/deepseek-harness/blob/99f6f02/packages/workflow/tool-ralph/src/index.ts#L404-L423)：worker 完成声明不是独立验收。
- [Eino delegation prompt](https://github.com/cloudwego/eino/blob/ebd616c/adk/prebuilt/deep/prompt.go#L28-L56)：以独立性、隔离性和上下文卸载判断委派；同文件的重复 ALWAYS/NEVER 段落作为反例。
- [Anthropic building effective agents](https://www.anthropic.com/engineering/building-effective-agents)：按可组合模式选 chain、parallel、orchestrator 或 evaluator。
- [Anthropic multi-agent research](https://www.anthropic.com/engineering/multi-agent-research-system)：委派目标、边界、来源/工具与返回格式必须明确，并用完整轨迹评测。

Pi 已从 `badlogic/pi-mono` 迁移到 `earendil-works/pi`；上述链接跟随原仓库重定向。Threadmill 的角色边界、VFS/Join 语义和用户要求优先于任何参考实现。
