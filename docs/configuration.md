# 配置与隔离

## 配置分层

提示词、Agent、工具和执行配置已经内置在二进制中，普通使用不再要求项目根目录存在 `threadmill.yaml`。模型设置按以下顺序覆盖，越靠后优先级越高：

1. 二进制内置默认值
2. 用户配置 `~/.threadmill/config.yaml`
3. 兼容旧项目的 `<workspace>/threadmill.yaml`
4. 项目配置 `<workspace>/.threadmill/config.yaml`
5. `-config` 指定的额外覆盖文件

用户配置由首次启动自动写入，格式如下：

```yaml
llm:
  provider: openai-responses
  base_url: https://api.openai.com/v1
  credential: personal
  model: gpt-5
  context_window: 272000
```

项目配置只需写要覆盖的字段，例如：

```yaml
# .threadmill/config.yaml
llm:
  model: another-model
  context_window: 200000
```

## 凭据配置

模型配置只保存凭据名，不保存 API key：

```yaml
llm:
  credential: opencode
```

密钥统一保存在用户目录的 `~/.threadmill/credentials.yaml`，同名字段对应模型配置中的凭据名：

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

Threadmill 默认只在可用的 `bwrap` 沙箱中执行 Agent 命令，不会静默降级到宿主执行。`bwrap` 保持挂载、用户和 PID 隔离，但默认共享宿主网络，并透传明确列出的代理、CA 与工具链配置；需要限制出站目标时，应由宿主防火墙或外层代理执行策略。如果宿主不能创建所需 namespace，可以为项目显式选择一个本地已有的 Docker 镜像：

```yaml
exec:
  container_image: golang:1.26.5-alpine
```

Docker 后端不会自动拉取镜像；容器禁用网络、使用只读根文件系统，并且只把当前 task 的 live workspace 挂载为可写目录。

如果 Threadmill 本身已经运行在 Pier 等可信的外层隔离边界中，并由外层负责进程、文件和出站网络策略，可以显式复用该边界：

```yaml
exec:
  external_sandbox: true
```

这不是宿主执行的自动降级。该模式仍为每个环境分配独立的 `HOME`/`TMPDIR`，并沿用相同的环境变量透传名单，不继承任意变量。不要在没有外层隔离的宿主上启用。

外层容器中的多 Agent 运行还应开启绝对路径隔离：

```yaml
exec:
  external_sandbox: true
  external_workspace_isolation: true
```

该选项只支持 Linux，要求外层容器向 Threadmill 进程授予 `SYS_ADMIN`
并放行 mount namespace。Threadmill 为每条命令创建私有 mount/PID
namespace，把项目的规范绝对路径映射到当前 VFS live，然后在执行模型命令前丢弃
`SYS_ADMIN`。权限或 namespace 不可用时以 `WORKSPACE_ISOLATION_UNAVAILABLE`
失败，不会回退到可绕过 VFS 的执行。命令结束时该 PID namespace 一并销毁，
因此该模式不用于跨多次 `bash` 调用保留后台进程。Harbor adapter 会自动提供这一最小权限和独立 VFS volume。

## 提示词结构

`threadmill.yaml` 目前有 10 份可配置提示词。角色提示词只描述职责、授权边界、工作方式和输出契约；工具参数与行为由 `tools` 的 description/schema 负责，避免重复。

| 配置项 | 使用者 | 负责内容 | 必须说明 |
| --- | --- | --- | --- |
| `prompts.default` | 未配置专用提示词的 Agent | 通用 ReAct 回退行为 | 何时调查/修改、工具真实性、授权边界、完成条件 |
| `prompts.compact` | 记忆整理调用 | 对话压缩为记忆节点 | 保留/丢弃范围、秘密过滤、节点类型/状态、归属和 JSON 契约 |
| `prompts.compact_json_reminder` | 压缩格式重试 | 修复不可解析输出 | 只输出完整 JSON 及唯一格式 |
| `prompts.drop_context_pressure` | 接近窗口上限的 Agent | 提醒释放当前上下文 | 不丢目标/约束/证据、操作可恢复 |
| `prompts.organize_query` | 子图整理请求 | 约束一次记忆检索 | 查询是数据、最小相关集合、目标 ID 和节点 ID 不得编造 |
| `agents.manager.system_prompt` | manager | 用户对话、协调图编排与真实目录阶段任务 | 创建阶段 task、运行中交流、真实目录验收、报告审计 |
| `agents.planner.system_prompt` | planner | 在任务工作区调查并产出执行计划 | 项目约束、执行图、验证和风险；隔离 task 的实验不保留，真实目录实验自行清理 |
| `agents.executor.system_prompt` | executor | 在任务工作区实施任务 | 目标优先级、最小改动、真实工具结果、验证、授权和结果报告 |
| `agents.verifier.system_prompt` | verifier | 在任务工作区独立验收 | PASS/FAIL/INCONCLUSIVE、逐项证据；阶段验收在真实目录，实验不得冒充持久修复 |
| `agents.subgraph_organizer.system_prompt` | subgraph organizer | 选择并挂接记忆节点 | 查询数据边界、搜索范围、最小集合和目标子图 |

运行时还会注入 manager 的最新协调图、用户消息与 task 报告、受保护的 task package、上游输出与 Input 阶段信息，以及压缩所需的已有记忆和对话。每个 task package 只包含分配给它的 Task Info 和明确关联的用户请求；创建关系和继承记忆不扩大授权。文件差异处理使用独立会话，原始候选材料不会作为正常角色的当前记忆提前注入。当前模块与接口映射见 [统一边设计](unified-edge-design.md)。修改提示词应在固定任务集上比较成功率、工具误用、证据完整性、token、延迟和费用，不能只凭文案判断。

