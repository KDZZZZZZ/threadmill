# Threadmill

Threadmill 是轻量级 Agent OS。

## 安装

Linux x86-64/ARM64 使用一键安装。它会请求 `sudo` 权限来安装运行时依赖，并在 Ubuntu AppArmor 限制 user namespace 时启用系统提供的 `bwrap` profile；沙箱探测不通过则安装失败。Threadmill、必要时使用的私有 Go 工具链和构建缓存放在 `~/.threadmill`，`~/.threadmill/bin` 会幂等写入当前 shell 的启动文件。VFS 会按当前权限和文件系统自动选择高性能后端：原生 OverlayFS、FUSE OverlayFS、reflink 或兼容复制：

```sh
curl -fsSL https://raw.githubusercontent.com/KDZZZZZZ/threadmill/dev-native/scripts/install.sh | sh
```

安装不会读取或写入模型密钥。打开新终端后，在任意项目目录首次运行：

```sh
threadmill
```

TUI 会询问 API 地址、模型、上下文窗口和凭据名，并用无回显输入读取 API key。配置保存到 `~/.threadmill/config.yaml`，密钥单独保存到权限为 `0600` 的 `~/.threadmill/credentials.yaml`。

在源码仓库中开发时仍可直接安装当前工作区版本：

```sh
GOBIN="$HOME/.threadmill/bin" go install ./cmd/threadmill
```

## 打开 CLI

在任意项目目录直接运行即可进入 TUI：

```sh
threadmill
```

首次交互配置完成后，也可以指定其他工作区，或执行一次无交互任务：

```sh
threadmill -C /path/to/project
threadmill -C /path/to/project -p "修复失败的测试"
threadmill -C /path/to/project -config /path/to/override.yaml
```

`-p` 不会启动首次配置交互；用于脚本前，请先运行一次 `threadmill`，或手动写好下面的配置和凭据文件。

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
  context_window: 128000
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

Threadmill 默认只在可用的 `bwrap` 沙箱中执行 Agent 命令，不会静默降级到宿主执行。`bwrap` 保持挂载、用户和 PID 隔离，但默认共享宿主网络，并只透传 HTTP(S) 代理和 CA 配置；需要限制出站目标时，应由宿主防火墙或外层代理执行策略。如果宿主不能创建所需 namespace，可以为项目显式选择一个本地已有的 Docker 镜像：

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

这不是宿主执行的自动降级。该模式仍为每个环境分配独立的 `HOME`/`TMPDIR`，并沿用相同的代理和 CA 白名单，不继承任意变量。不要在没有外层隔离的宿主上启用。

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

运行时还会动态拼入 5 类上下文，它们不是独立配置项：manager 的最新协调图；manager 专属的用户消息与 task 报告；task 启动包；planner → executor → verifier 的上游输出，以及待处理 Join 的紧凑 session 元数据（候选完整输出和文件只通过 `join` 按需读取）；压缩调用中的已有记忆、可选子图和待整理对话。修改提示词时应在固定任务集上比较任务成功率、工具误用、验证完整性、token、延迟和费用，不能只凭文案判断效果。
