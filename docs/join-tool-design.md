# 旧 Join 工具设计迁移记录

> 状态：历史设计，已由 [统一边与文件优先合入设计 v0.4](unified-edge-design.md) 替代。本文不再规定当前运行时行为，也不授权恢复 `spawn`/`join` 边种类、root 调度或提前合入记忆。

旧协议在 [迁移前基线 `dbcd2a0`](https://github.com/KDZZZZZZ/threadmill/blob/dbcd2a092d6a1bb76471c8fdcb672454afaf35ac/docs/join-tool-design.md) 中保留。当时 `join` 会话组织文件候选，而记忆合入与文件选择的完成边界分开。统一边实现用同一 Input 批次收集全部固定的文件与记忆出口，共同部分直接继承，只处理差异，并严格先文件、后记忆。

| 旧对象或行为 | 当前替代 |
| --- | --- |
| `sequence / spawn / join` 边种类 | 只有 `Edge{From, To}`；角色顺序、分支、汇合都由普通 DAG 表达 |
| `roots / spawns` 编排、隐式 root 继承链 | `tasks / edges`；无 root 或按创建顺序继承 |
| `join` 工具与 Join Session | [`input` 工具](../internal/coordination/input_tool.go)与 [`InputProgress`](../internal/coordination/progress.go) |
| `join finish` 即候选会话完成 | `input finish` 只持久化文件决策完成；运行时冻结文件、整理记忆后提交成对 `ready` |
| 把来源记忆提前 Merge 到目标 | 固定来源 → 交集/差异 → 文件处理 → 隔离记忆差异整理 → 成对可见 |
| Help 单独返回并合入 | 暂停/恢复节点及普通边；只有显式指向 resume 的来源才需要等待 |
| 子树统一等待、取消和清理 | 每个 task 激活独立；没有返回边的 task 可独立运行，持久 task 可 idle 后继续 |
| 旧 `Finished`、`Merged` 或 task `done` | 不能映射成新 Input ready；Graph/Progress 版本 1 明确拒绝旧格式 |

VFS 内部的 `InputChanges`、`ApplyInput` 名称仍是文件检查和安全应用原语；它们消费 `PrepareInput` 生成的差异候选，不定义协调图的边或第二套合入协议。来源保持只读，`safe` 冲突时整次零写入，`replace` 与部分采用必须显式决定；不增加自动文本合并。

当前实现、恢复限制与验证入口统一维护在 [统一边设计](unified-edge-design.md)。这次没有旧状态自动转换器，也没有新增快照垃圾回收。
