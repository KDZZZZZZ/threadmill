# Multi-Agent Vibe Coding Control Plane 总体架构

版本：v0.2  
状态：Draft  
定位：本文件只描述产品判断、总体架构、模块简述和模块间关系。各模块详细设计见文末链接。

---

## 1. 产品判断

当前 vibe coding 的核心瓶颈不是“agent 不够聪明”，而是 **人类被迫承担了调度系统的工作**。

当多个 agent 同时工作时，用户通常需要手动处理这些事情：

1. 重新告诉新 session 当前项目状态。
2. 解释其他 agent 正在做什么。
3. 手动拆任务。
4. 手动同步上下文。
5. 手动避免 worktree 和代码冲突。
6. 手动判断哪个结果可以合并。

这个系统的产品判断是：

> 用户不应该管理 session，也不应该手动调度每个 agent。用户只应该表达目标、预算和并发意图；系统负责把目标拆成 task graph，并调度可用 agent 完成、验证和合并。

因此，产品形态不是“多开几个 Claude/Codex 窗口”，而是一个 **Multi-Agent Control Plane**：

```text
用户输入：
  我需要什么 + 我愿意投入多少钱/多少 agent/多少时间

系统负责：
  拆任务、分配 agent、注入上下文、隔离 worktree、验收、处理冲突、合并
```

---

## 2. 核心目标

1. **需求和并发解耦**  
   用户发布新任务，不等于手动开启新 agent；用户点击 `agent +1`，也不等于给某个 agent 分配具体任务。

2. **统一 CLI agent runtime**  
   系统扫描并接入 Claude Code、Codex、Gemini CLI 等 headless CLI agent，包装成统一 worker。

3. **用 task graph 管理工作，而不是用 session 管理工作**  
   root task 表示用户目标；child task 表示系统拆出的可执行工作单元。

4. **用 ctxlib 管理项目记忆，而不是依赖 session memory**  
   所有有价值的设计、判断、验收、失败和冲突信息沉淀到结构化上下文库。

5. **每个 task 用 worktree 隔离执行**  
   agent 不直接修改 main；verify 通过后进入 merge queue。

6. **复杂任务允许通过创建子任务作为交付**  
   planner 不必一次性解决所有细节，可以把大任务拆成可验收的 child tasks。

7. **verify 是进入项目事实的闸门**  
   task 只有通过验收后才允许 merge；失败则回到 plan 循环。

8. **并发冲突由系统协调**  
   verify/merge 阶段检查活跃 task，如果冲突则广播 conflict context 给相关 active task，避免互相等待和死锁。

---

## 3. 非目标

本系统不追求：

1. 让所有 CLI agent 拥有完全一致的能力。
2. 把所有历史 session 原样塞进新 agent 上下文。
3. 让 agent 自由修改 main branch。
4. 让 execute agent 在没有 replan 的情况下任意扩大任务范围。
5. 用 embedding-only memory 替代结构化上下文管理。
6. 让人类继续手动协调每个 agent 的具体工作。

---

## 4. 总体架构

```text
┌──────────────────────────────────────────────┐
│                   Human UI                   │
│  目标输入 / agent+1 / 预算 / 进度 / 验收       │
└───────────────────────┬──────────────────────┘
                        │
                        ▼
┌──────────────────────────────────────────────┐
│                 Control Plane                │
│  Scheduler / Policy / Budget / Orchestration │
└───────┬──────────────┬──────────────┬────────┘
        │              │              │
        ▼              ▼              ▼
┌──────────────┐ ┌──────────────┐ ┌──────────────┐
│ Task Graph   │ │ Context Lib  │ │ Agent Runtime│
│ 状态与依赖    │ │ 项目级记忆    │ │ CLI Wrappers │
└──────┬───────┘ └──────┬───────┘ └──────┬───────┘
       │                │                │
       ▼                ▼                ▼
┌──────────────────────────────────────────────┐
│          Workspace / Git / Merge Queue       │
│   Worktree Isolation / Verify / Conflict     │
└───────────────────────┬──────────────────────┘
                        ▼
┌──────────────────────────────────────────────┐
│          Event Log / Artifact Store          │
│     Logs / Results / Diff / Test Evidence    │
└──────────────────────────────────────────────┘
```

一句话概括：

```text
Task Graph 决定现在该做什么。
Context Lib 决定 agent 应该知道什么。
Agent Runtime 决定谁来做。
Workspace / Git 决定在哪里做。
Verifier 和 Merge Queue 决定什么能进入项目事实。
Event Log 决定系统如何追溯和复盘。
```

---

## 5. 模块简述

## 5.1 Human UI

Human UI 面向用户，不暴露底层 session，而暴露目标、预算、agent capacity、task graph、active agents、verify 状态和 merge 状态。

核心操作：

```text
- 创建目标
- 增加/减少 agent 数量
- 调整预算
- 查看 task graph
- 查看阻塞和冲突
- 批准高风险操作
- 查看验收结果
```

---

## 5.2 Control Plane

Control Plane 是调度中枢，负责把用户目标转成 task graph，并根据预算、优先级、依赖和 agent capacity 启动具体 phase。

它不直接实现业务逻辑，而是协调这些模块：

```text
Control Plane -> Task Graph：创建 root task / child task，更新状态。
Control Plane -> Context Lib：为某个 task phase 选择 context pack。
Control Plane -> Agent Runtime：启动 planner / executor / verifier。
Control Plane -> Workspace：创建 worktree，进入 verify 和 merge。
Control Plane -> Event Log：记录所有关键事件。
```

---

## 5.3 Task Graph

Task Graph 是工作结构的核心。

- **root task**：用户直接提出的顶层目标。
- **child task**：planner 或系统从 root task 拆出的可执行任务。
- **blocked task**：等待子任务、依赖、冲突处理或人类决策的任务。

复杂任务可以通过创建 child tasks 作为合法交付。父 task 不因为创建子任务而完成，而是进入 blocked/waiting 状态，等子任务完成后再重新验收整体目标。

详见：[Task Graph 详细设计](./task-graph.md)。

---

## 5.4 Agent Runtime

Agent Runtime 将 Claude Code、Codex、Gemini CLI 等不同 CLI agent 包装成统一 worker。

统一不是指能力完全相同，而是每个 agent 暴露 capability profile：

```text
- 是否支持 headless
- 是否支持 structured output
- 是否能编辑文件
- 是否能运行 shell
- 是否支持 MCP
- 上下文窗口和成本模型
- 适合承担 planner/executor/verifier 中哪些角色
```

详见：[Agent Runtime 详细设计](./agent-runtime.md)。

---

## 5.5 Context Lib

Context Lib 是项目级上下文库，用来替代 session memory。

它存储经过提取、标注和验证的项目记忆，例如：

```text
- 架构决策
- 模块摘要
- 任务摘要
- 验收结果
- 失败原因
- 冲突分析
- 用户偏好
- rejected approaches
```

新 agent 启动时不会加载全量 ctxlib，而是由 Context Curator 根据 task、phase、scope、confidence、freshness 和 risk 选择有限 context pack。

详见：[Context Lib 详细设计](./ctxlib.md)。

---

## 5.6 Workspace / Git / Merge Queue

Workspace / Git 模块负责 worktree 隔离、diff 观察、verify、merge queue 和冲突协调。

基本原则：

```text
- 每个 task attempt 独立 worktree。
- agent 不直接修改 main。
- verify 通过后才进入 merge queue。
- merge 前检查 active conflicts。
- merge 后产生新的项目事实并写入 ctxlib。
```

详见：[Workspace 与 Merge Queue 详细设计](./workspace-merge.md)。

---

## 5.7 Scheduler / Budget

Scheduler 根据 task graph、agent capacity、预算、风险和依赖关系决定下一步启动什么。

用户点击 `agent +1` 时，只是增加 worker capacity；Scheduler 决定新增 worker 去执行哪个 task phase。

预算不仅是金钱，还包括：

```text
- token
- 时间
- 并发数
- retry 次数
- verify 强度
- shell 执行成本
```

详见：[Scheduler 与 Budget 详细设计](./scheduler-budget.md)。

---

## 5.8 Event Log / Artifact Store

Event Log 是系统事实来源。task 表、ctxlib 索引、UI 状态都可以视为 event log 的 projection。

Artifact Store 保存大对象：

```text
- agent transcript
- tool output
- test output
- diff patch
- screenshots
- benchmark result
```

详见：[Event Log 与 Artifact Store 详细设计](./event-artifact-store.md)。

---

## 6. 模块间关系

## 6.1 创建新目标

```text
Human UI
  -> Control Plane
  -> Task Graph 创建 root task
  -> Context Lib 选择初始上下文
  -> Scheduler 决定何时启动 planner
```

关键判断：创建 root task 只是把用户目标放入系统，不等于立即开一个新 session。

---

## 6.2 增加 agent 数量

```text
Human UI: agent +1
  -> Control Plane 增加 worker capacity
  -> Scheduler 选择下一个可运行 task phase
  -> Agent Runtime 启动对应 CLI agent
```

关键判断：增加 agent 是增加系统吞吐，不是让用户手动指定“这个新 agent 去做什么”。

---

## 6.3 执行一个 task

```text
Task Graph 提供 task contract
  -> Context Lib 生成 context pack
  -> Workspace 创建 worktree
  -> Agent Runtime 启动 plan / execute / verify agent
  -> Event Log 记录过程
  -> Verify 通过后进入 Merge Queue
```

---

## 6.4 复杂任务拆解

```text
Planner 发现 root task 过大
  -> 创建 child tasks
  -> 父 task 进入 blocked / waiting_children
  -> Scheduler 调度 child tasks
  -> child tasks 完成后父 task 重新验收
```

关键判断：创建 child tasks 是复杂任务的合法交付，不是失败。

---

## 6.5 上下文沉淀与再利用

```text
Agent 输出 summary / verify failure / merge result
  -> Event Log 保存原始事件
  -> Context Curator 提取 context block
  -> Context Lib 标注、去重、supersede
  -> 后续 task phase 选择 context pack
```

关键判断：长期记忆属于 ctxlib，不属于某个 agent session。

---

## 6.6 并发冲突协调

```text
Task A verify passed
  -> Merge Queue 检查 active tasks
  -> 发现 Task B 有 write set 重叠
  -> 广播 conflict context 给 Task B
  -> Task B 单边 replan 或 adapt
```

关键判断：已经 verify passed 的 task 优先，仍在执行的 task 负责适配，避免双方互相等待。

---

## 7. 架构不变量

```text
1. task 未通过 verify 不得 merge。
2. agent 不拥有长期记忆，ctxlib 拥有长期记忆。
3. agent 启动不加载全量 ctxlib。
4. 每个 task attempt 独立 worktree。
5. execute 不直接修改 main。
6. verify agent 不自我批准 execute 结果。
7. 创建 child tasks 是复杂任务的合法交付。
8. blocked 不是 failed。
9. merge 后必须产生可追溯事件和上下文沉淀。
10. 用户控制目标和资源，系统控制调度细节。
```

---

## 8. MVP 分期总览

```text
MVP 0：单机 Task Graph + Worktree
  跑通 task -> worktree -> plan -> execute -> verify -> merge 的最小闭环。

MVP 1：Context Pack
  用结构化 context block 替代人工 session handoff。

MVP 2：运行时 ctxlib 检索
  允许 agent 在受控协议下补充上下文，并在必要时触发 replan。

MVP 3：Agent Runtime + Worker Pool
  接入多个 CLI agent，实现 agent+1。

MVP 4：Conflict-Aware Merge Queue
  支持多 agent 安全并发和冲突广播。

MVP 5：Context Curator
  自动沉淀项目记忆、标注上下文、识别 supersede 和失败经验。
```

---

## 9. 详细设计文档

- [Task Graph 详细设计](./task-graph.md)
- [Agent Runtime 详细设计](./agent-runtime.md)
- [Context Lib 详细设计](./ctxlib.md)
- [Workspace 与 Merge Queue 详细设计](./workspace-merge.md)
- [Scheduler 与 Budget 详细设计](./scheduler-budget.md)
- [Event Log 与 Artifact Store 详细设计](./event-artifact-store.md)
