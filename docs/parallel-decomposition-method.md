# Threadmill 并行分解方法（IPD Loop）

> 目标：把一个高层结果递归精化为可执行图，同时在“独立可验收”处停止。并行度由交付边界、依赖与写入面推导，不由文件数、步骤数或机器槽位推导。

## 1. 运行时事实

| 事实 | 含义 |
| --- | --- |
| root 按出现顺序串行 | 把并行工作拆成多个 root 只会增加关键路径 |
| 后 root 从前 root 的持久环境 fork | root 是继承链，不是并行兄弟；同一交付必须只有一个 owner |
| helper 在 task 内并行并 join 回请求者 | task 内 helper 是唯一并行面；请求者是唯一 integration owner |
| Planner 与 Verifier 的工作区一次性，Executor 的工作区形成 task 快照 | Planner 只保留计划，Verifier 只保留报告；每个结束 task 的文件状态独立归档，Manager 验收后只发布所选的一个快照 |
| Planner、Executor、Verifier 都可请求 help，Manager 响应真实请求 | 三者只通过 `coordination_requestHelp` 提交建议；Planner 并行取得规划证据/候选，Executor 并行实现，Verifier 并行取证；Manager 校验无环来源后决定是否物化 |
| join 候选默认只读，不自动应用 | 每个来源必须显式 apply 或 discard；finish 不等于验收通过 |
| task 结束不自动改真实项目 | `done`、Verifier verdict 与发布是三件事；Manager 用协调工具选择一个终态快照，协调图持久记录发布意图与最终引用，失败按原 task 重试 |

代码事实分别位于 `internal/manager`、`internal/coordination`、`internal/vfs` 和 `internal/agent`。若运行机制改变，先更新本节，再改提示词。

## 2. 语义精化与认知闭包

### 2.1 从目标递归做语义精化

大型任务不是步骤列表，而是把压缩的高层目标逐层展开为可执行语义。每一层只做同一种工作：

1. 说明目的，以及“条件 → 可观察结果”。
2. 说明这一层承诺什么、不承诺什么、保持哪些不变量、需要下层提供哪些能力。
3. 说明谁拥有这部分知识，以及怎样不依赖实现细节验收它。
4. 用稳定、正交、最小而完整的概念保留任务信息；不要把当前实现偶然具有的顺序、存储或机制写成语义。
5. 把因同一原因变化的决定放在一起。上层拥有策略与语义约束，下层拥有机制；只有语义变化穿过知识边界，内部实现变化应停在边界内。

未形成契约的节点不能继续向下实现，否则只是把歧义传播给下一层。

### 2.2 认知闭包决定继续精化、委派或直接实现

对每个节点问三件事：上层是否知道如何使用它，是否知道如何验收成败，并且无需知道内部实现也能完成上层决策。

- 任一项不成立：继续语义精化，不委派歧义。
- 全部成立，且下层是自包含、值得卸载的实现域：委派。
- 全部成立，且只是简单叶子或成熟原语：直接实现或复用。

这叫认知闭包。扩展性和并行度是正确抽象的副产品，不是预设目标。闭包只决定语义上能否委派；工具层再用 `admission_reason` 判断此次委派是否值得：

1. `critical_path`：它能与另一个 ready 单元并行，从而缩短依赖链。
2. `context_offload`：接口、使用方式和验收方式已冻结；列出被隔离的内部问题，且答案不改变父级决策。
3. `race`：`race_basis` 取 `user_requested` 或 `unresolved_alternatives`；后者给出至少两个不同候选、同一 gate、唯一 adjudicator 和不能先用更小实验消歧的原因。

短例：同一知识、同一变化原因留在一起，与文件数量无关；还说不清“正确”时继续精化，闭合后复杂域委派、简单叶子直接做；两个候选只有在回答同一问题、共用同一验收且最多采纳一个时才 race。

## 3. IPD Loop

### P0 完成定义

- 逐字恢复累计 Task Info。
- 每条硬要求写成：条件、可观察结果、负向边界、最强直接证据入口。
- 给每条契约标注一个或多个类型：结论型、行为变更型、行为保持型、评测型；任务门禁取并集，不因标签碎拆。

### P1 语义与知识边界

- 对每个节点写目的、承诺、非承诺、不变量、所需能力、知识 owner 和实现无关的验收。
- 把同因变化的知识放在一起，标出哪些机制变化必须在边界内停止传播。
- 不因关键词相似新建平行抽象，不为未来假设添加扩展点；未闭合节点继续精化。

### P2 候选单元

从已认知闭合、值得卸载的节点产生候选，不从文件、函数或操作步骤枚举候选。每个候选仍必须完整写出工具所需契约：

- `admission_reason`；
- `id` 与目标；
- 已知输入和路径；
- 不可违反的约束；
- 独立交付物；
- 写入面；
- evidence recipe；可执行行为写命令、预期观察和退出码，结论/文档可用可定位来源或解析结果；
- 依赖、输出格式、明确不做什么、阻塞与返回条件。

没有独立结果的内容只是步骤，不是任务。包含多个独立结果的单元标 `expandable`；已经自洽的结果标 `leaf`。

### P3 关系与冲突

只使用四种关系：

| 关系 | 使用条件 | 合流 |
| --- | --- | --- |
| `section` | 不同且可独立验收的交付 | 唯一 integration owner |
| `pipeline` | 消费者确实读取生产者产物 | 依赖满足后进入下一 frontier |
| `hypothesis` | 诊断的互斥根因或独立证据渠道 | 唯一裁决者选择被证实解释 |
| `race` | `race_basis=user_requested` 或 `unresolved_alternatives`；后者给出至少两个不同候选、同一 gate、唯一 adjudicator 和不能先用更小实验消歧的原因 | 隔离运行，最多采纳一个 |

对每个单元列文件区域、符号、状态、配置和生成物写入面。同 wave 的互补单元有交集时，只能选择：合并、排序，或先建立稳定契约/fixture/schema 作为前置接缝。不能用伪依赖掩盖写入冲突。

Join 的采纳粒度是路径，不是符号。同一路径的互补候选先 `inspect/compare`，再由 integration owner 用正常 read/edit/write 人工合成；不得用 `replace` 覆盖已合入结果。人工吸收后带理由 discard 来源，再 finish 并运行组合门禁。

### P4 Frontier 与合流

- 并行度是 ready 独立结果的数量，不是预设目标。
- `ready frontier` 恰好包含依赖已满足的单元。
- 同一 frontier 的互补单元须写入不冲突；合规 race 候选在隔离工作区可共享写入边界，并进入同一 concurrency group。
- 共享前置只做一次，完成后立即 fan-out。
- 只展开当前 frontier；未知后代由子任务自己的 Planner 继续精化。
- 所有结果只在一个 integration owner 合流；合流后运行组合门禁。

自然 helper 数就是当前 frontier 中通过分派门槛的单元数。没有自然并行面时写 `split: none`，并指出“一个交付物 / 一个共享状态 / 一个验收边界”中的实际原因；不需要逐轴证明，也不能用“任务小”代替边界。

### P5 审计

- **I1 覆盖唯一**：每条累计契约只有一个最终 acceptance/integration owner；race candidates 只拥有候选产物，不拥有最终契约。
- **I2 写入隔离**：同 wave 互补单元写入交集为 0；race 豁免，在隔离工作区可重叠写入/验收，只有采纳与合流经过唯一裁决点。
- **I3 独立可判定**：已声明依赖物化后，integration owner 或 helper 自带 verifier 不读取未声明兄弟结果即可判定。
- DAG 无环，ready frontier 精确，唯一 integration owner 明确。
- 每条硬契约有一项最强直接门禁；先覆盖最高假绿风险，不重复同义代理检查。
- Executor 无需重新选择架构、补写契约或重新拆分即可开始。

任一项失败时回到最早相关阶段修正，不把审计写成与执行图重复的第二套计划。

## 4. 角色 help 与 Planner / Executor 交接

请求者始终是唯一综合与裁决 owner。Planner helper 只回答会改变 owner、跨边界契约或门禁的独立未知；Executor helper 交付可独立验收的实现；Verifier helper 只交付可重取的验收观察，不能修复基线或把 FAIL 变成 PASS。Manager 只从同一 task tree 中已有输出且不会形成 join cycle 的节点物化 helper；因此首个 root Planner 通常没有合法来源，而嵌套 Planner 可复用祖先输出，Verifier 可复用本 task 的 Planner/Executor 输出。没有合法来源时继续本角色工作，不新增边或伪造依赖。

Planner 只交付四段：

1. `完成定义`：任务类型与逐条累计契约。
2. `执行图 / Help Executor 计划`：完整单元契约、关系、waves、ready frontier、并发组、最终 owner、race producers 和 integration owner。
3. `集成与最终门禁`：合流动作及每个 evidence recipe 可证伪的契约。
4. `未知项`：仍需 Executor 验证的假设、替代路径和未覆盖面。

Executor 对当前 ready frontier 只提交一次 help 请求，不按 child 分批，不改写依赖。该请求只是编排建议，不直接修改协调图。Manager 可以拒绝未通过 I1/I2/I3 的单元，但不静默替 Planner 重新设计；所有 root/helper 改图都经 `coordination_orchestrate`。未物化部分由 Executor 按同一图完成。

## 5. 按任务类型选门禁

| 类型 | 门禁 |
| --- | --- |
| 结论型 | 无授权持久交付的纯结论任务：原始来源、复现或代码路径；相对 task 继承基线零新增持久写且不回退 |
| 行为变更型 | 契约证据；代码有稳定入口时加持久回归，文档/配置做 parse/render/schema/reference 检查，产物参加构建时才构建 |
| 行为保持型 | 目标结构检查、明示不变量、兼容性与受影响回归 |
| 评测型 | 冻结 benchmark 数据、harness/基线、环境、模型非秘密配置、评分协议和日志；submission 仅按 Task Info 修改，聚合可复算 |

门禁失败必须能证伪 Task Info 条件或本次 diff 触及的既有行为；否则记录不适用，不让无关门禁阻断。

## 6. 修复图

每份 Verifier 报告至多触发一个 repair root，且只处理“可由工作区改动消除并已有复现证据”的缺陷，不预排后续修复。新报告给出新证据并改变修复目标时，才可追加下一 root。repair root 继承累计契约与已保留实现，不回退实现。

第一轮直接定位并修根因。只有同型缺陷再次出现且存在多个互斥解释时，repair Executor 才请求一个 hypothesis frontier；所有诊断结果 join 回该 Executor，由它裁决、实现并跑组合门禁。环境、权限、上游服务或 benchmark oracle 问题只记录复验条件，不创建工作区修复 root。

## 7. 短例

- 一个语义决定跨越多个文件，仍是一个单元；一个文件承载两个互不改变定义的结果，也可以是两个单元。
- 还无法说明下层怎样算“正确”时，由当前 owner 继续精化；使用、失败和验收都已闭合后，复杂域才成为 helper 候选。
- 多个方案只有在回答同一问题、使用同一 gate 且由一个 owner 最多采纳一个时才是 race。

这些例子只说明判断方式；具体字段、race 和 join 行为仍以工具协议为准。

## 8. 验收指标

- 累计契约覆盖率 = 100%。
- 重复 owner = 0。
- same-wave 互补写入交集 = 0。
- DAG 环 = 0；ready frontier 误入/漏入 = 0。
- helper 独立验收覆盖率 = 100%。
- join 冲突率、重复劳动率、repair root 数、端到端时间和 token 与基线比较。

使用相同任务与环境做 A/B；一次只改一组提示规则。没有轨迹证据时，不把“计划更复杂”或“agent 更多”记为改进。

## 9. 参考

- [用户提供的“软件工程可扩展性”讨论](https://chatgpt.com/share/6a904448-6700-83e8-9367-9e46da73470f)：语义精化、知识边界、认知闭包，以及“扩展性是正确抽象的副产品”。
- [OpenAI Model guidance](https://developers.openai.com/api/docs/guides/latest-model)
- [Anthropic: Building effective agents](https://www.anthropic.com/engineering/building-effective-agents)
- [Anthropic: Multi-agent research system](https://www.anthropic.com/engineering/multi-agent-research-system)
- [Eino task delegation](https://github.com/cloudwego/eino/blob/ebd616c/adk/prebuilt/deep/prompt.go#L28-L56)
- [DeepSeek Harness team task contracts](https://github.com/deepseek-ai/deepseek-harness/blob/99f6f02/packages/experimental/tool-agent-team/src/index.ts#L173-L195)
- [Design Structure Matrix](https://dsmweb.org/introduction-to-dsm/)
