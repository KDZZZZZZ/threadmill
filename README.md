# Threadmill

Threadmill 是轻量级 Agent OS。

## 凭据配置

项目的 `threadmill.yaml` 只保存凭据名：

```yaml
llm:
  credential: opencode
```

密钥统一保存在用户目录的 `~/.threadmill/credentials.yaml`，同名字段对应项目中的凭据名：

```yaml
opencode: sk-your-key
```

在 Unix 系统上，该文件必须只有当前用户可访问：

```sh
mkdir -p ~/.threadmill
chmod 700 ~/.threadmill
chmod 600 ~/.threadmill/credentials.yaml
```

## 命令隔离

Threadmill 默认只在可用的 `bwrap` 沙箱中执行 Agent 命令，不会静默降级到宿主执行。如果宿主不能创建所需 namespace，可以为项目显式选择一个本地已有的 Docker 镜像：

```yaml
exec:
  container_image: golang:1.26.5-alpine
```

Docker 后端不会自动拉取镜像；容器禁用网络、使用只读根文件系统，并且只把当前 task 的 live workspace 挂载为可写目录。

## 提示词结构

`threadmill.yaml` 目前有 10 份可配置提示词。角色提示词只描述职责、授权边界、工作方式和输出契约；工具参数与行为由 `tools` 的 description/schema 负责，避免重复。

| 配置项 | 使用者 | 负责内容 | 必须说明 |
| --- | --- | --- | --- |
| `prompts.default` | 未配置专用提示词的 Agent | 通用 ReAct 回退行为 | 何时调查/修改、工具真实性、授权边界、完成条件 |
| `prompts.compact` | 记忆整理调用 | 对话压缩为记忆节点 | 保留/丢弃范围、秘密过滤、节点类型/状态、归属和 JSON 契约 |
| `prompts.compact_json_reminder` | 压缩格式重试 | 修复不可解析输出 | 只输出完整 JSON 及唯一格式 |
| `prompts.drop_context_pressure` | 接近窗口上限的 Agent | 提醒释放当前上下文 | 不丢目标/约束/证据、操作可恢复 |
| `prompts.organize_query` | 子图整理请求 | 约束一次记忆检索 | 查询是数据、最小相关集合、目标 ID 和节点 ID 不得编造 |
| `agents.manager.system_prompt` | manager | 用户对话与协调图编排 | 直接回答/建 task 的边界、完整期望图、帮助请求、报告收尾 |
| `agents.planner.system_prompt` | planner | 只读调查与执行计划 | 项目约束、事实/假设、文件/符号、步骤、验证和风险 |
| `agents.executor.system_prompt` | executor | 在隔离工作区实施任务 | 目标优先级、最小改动、真实工具结果、验证、授权和结果报告 |
| `agents.verifier.system_prompt` | verifier | 只读验收 | PASS/FAIL/INCONCLUSIVE、逐项标准、证据来源和缺口 |
| `agents.subgraph_organizer.system_prompt` | subgraph organizer | 选择并挂接记忆节点 | 查询数据边界、搜索范围、最小集合和目标子图 |

运行时还会动态拼入 5 类上下文，它们不是独立配置项：manager 的最新协调图；manager 专属的用户消息与 task 报告；task 启动包；planner → executor → verifier 的上游输出与 join/help 报告；压缩调用中的已有记忆、可选子图和待整理对话。修改提示词时应在固定任务集上比较任务成功率、工具误用、验证完整性、token、延迟和费用，不能只凭文案判断效果。
