# Context Lib 详细设计

版本：v0.2
状态：Draft
定位：ctxlib 用结构化项目记忆取代 session memory。本文件给出一个**小内核 + 明确扩展缝**的原型：核心模型和接口尽量小而稳定，richer 能力(多维筛选、排序、embedding、过时判断等)都作为可插拔扩展，不进内核。

---

## 1. 设计原则

```text
1. 小内核：ContextBlock 只保留少量稳定字段，其余用开放结构表达。
2. 单一来源：ctxlib 只从 Event Log / Artifact 构建，不接受 agent 直接写。
3. 单一入口：对外只有 Ctx Agent，且只暴露 pack / query 两个只读操作。
4. 缝在接口上：抽取、评分、存储都是可替换接口，扩展不改内核。
5. 一切可追溯：每个 block 都指回它的来源事件 / artifact。
```

一句话：**内核只回答"有哪些记忆、怎么取"，"记忆有多好、怎么排"交给可替换的策略。**

---

## 2. 核心数据模型

只有一个核心类型。可扩展性来自两处：`scope` 用**前缀约定的开放标签**，`attributes` 用**开放键值**。新增维度不改 schema。

```ts
ContextBlock {
  id: string
  kind: string            // 开放字符串。原型约定：decision / summary / failure /
                          // conflict / preference / rejected …；新增类型不改内核。

  text: string            // 可直接注入的内容（摘要）。更长的正文/原始日志放 refs。

  scope: string[]         // 开放标签，用前缀表达"它关于什么"：
                          //   task:T123  module:ctxlib  file:src/x.ts
                          //   phase:plan tag:api  ...
                          // 新增一个维度 = 约定一个新前缀，无需改字段。

  outdated: boolean       // 是否已过时。原型不表达版本链路，只标记可用性。

  source_refs: string[]   // 指向 Event Log 事件 / Artifact 的证据，保证可追溯

  attributes: Record<string, unknown>
                          // 扩展位。confidence / freshness / importance /
                          // visibility / risk 等都放这里。
                          // 原型可以只填 confidence 和 created_at。

  created_at: string
}
```

为什么这么设计：

```text
- 原来的 repo/module/file/symbol/task/phase_scope 六个字段 -> 合并成 scope 标签。
- 原来的 confidence/importance/freshness/validity/visibility/risk 等 -> 放 attributes。
- 结果：内核字段从 ~20 个降到 7 个；加新维度不再是 schema 变更。
```

---

## 3. Ctx Agent：唯一入口

Ctx Agent 是 ctxlib 的唯一受控访问入口，也是唯一写入者。它以 **Event Log 为唯一数据来源**构建 ctxlib，对外只提供两个只读操作。其他 agent 不直接读写底层存储，也不推送内容——它们的活动被自动记入 log，由 Ctx Agent 从 log 提炼。

```ts
// 启动前：为某个 task/phase 组装 context pack
pack(req: {
  task_id: string
  phase: string
  budget: number          // token 预算
}) -> ContextResult

// 运行中：其他 agent 受控查询
query(req: {
  task_id: string
  phase: string
  intent?: string         // 可选，缩小检索目的（开放字符串）
  scope?: string[]        // 可选，限定范围标签
  budget: number
}) -> ContextResult

ContextResult {
  blocks: ContextBlock[]      // 已选中、可注入
  omitted: string[]           // 相关但因预算未注入的 block id（agent 可按需再查）
  note?: "replan" | "human_decision" | null   // 发现矛盾时的建议
}
```

对外没有 write / outdated 标记操作。要沉淀记忆的 agent 只管正常做事,活动进 log,Ctx Agent 负责提炼；某条记忆是否过时也由 Ctx Agent 从 log 中判断。

---

## 4. 三个可替换接口（扩展缝）

内核只依赖这三个接口的**签名**，不依赖其实现。原型给最简实现，之后各自独立演进。

### 4.1 Extractor：log -> block（怎么产生记忆）

```ts
Extractor = (event: Event) => ContextBlock[]
```

- 原型：几条规则型 extractor（verify 失败、merge 结果、人类需求）。
- 扩展：新增来源 = 新增一个 extractor 注册进来，内核不变。

### 4.2 Selector：给定请求挑 block（怎么取记忆）

```ts
Selector = (candidates: ContextBlock[], req: Request) => Ranked[]
```

- 原型：`scope` 标签重叠 + 新鲜度 + 过滤 `outdated=true`，够用。
- 扩展：换成多路召回 + 打分重排 + embedding，只替换 Selector，接口不变。

### 4.3 Store：底层存取（记忆存哪）

```ts
Store {
  put(block): void
  get(id): ContextBlock
  find(filter): ContextBlock[]   // 按 kind/scope/outdated 粗筛
}
```

- 原型：内存或单文件 / SQLite。
- 扩展：换成向量库 / 外部服务，只替换 Store。

---

## 5. 数据流

```text
runtime 自动记录 agent 活动 / 状态变化
  -> Event Log
       -> Extractor 提炼出 ContextBlock（去重、必要时标记旧 block 为 outdated）
       -> Store 保存

Ctx Agent.pack / query
  -> Store.find 粗筛候选
  -> Selector 排序 + 裁到 budget
  -> ContextResult（blocks + omitted + note）
```

写入路径(Extractor->Store)只发生在 Ctx Agent 内部,由 log 驱动;对外只有 pack/query。

---

## 6. 不变量

```text
1. ctxlib 只从 Event Log / Artifact 构建，不接受 agent 直接写。
2. 只有 Ctx Agent 能访问底层存储；对外只有 pack / query 两个只读操作。
3. 每个 block 必须有 source_refs，可追溯到来源事件 / artifact。
4. outdated block 默认不进 pack（除非显式查历史）。
5. 扩展通过 Extractor / Selector / Store 三个接口完成，不改核心模型。
6. 访问 ctxlib 的行为本身也被自动记入 log。
```

---

## 7. 原型如何长成完整设计（扩展映射）

说明"覆盖没减少"，只是从内核挪到了扩展缝：

```text
需要的能力            原型落点                       扩展方向
------------------   ---------------------------   -----------------------------
更多 context 类型     kind 加约定值                  分类体系 / 校验
更多范围维度          scope 加前缀                   scope 索引 / 命名规范
置信度/新鲜度/重要度   attributes 键                  打分权重、时间衰减
可见性/风险控制        attributes 键                  Selector 里做权限过滤
多路召回 + 重排        Selector 换实现                embedding + rerank
过时判断              outdated 字段 + Selector       矛盾检测、历史保留
运行时检索协议         query 的 intent/scope          结构化 intent 词表
不同存储后端          Store 换实现                    向量库 / 外部服务
```

内核（第 2、3 节）保持稳定；以上都在不改内核的前提下增量演进。
