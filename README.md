# Threadmill

Threadmill 是轻量级 Agent OS。

## 安装

Linux x86-64/ARM64 支持一键安装。**先进入要使用的项目目录**，或用 `THREADMILL_PROJECT_DIR` 指定它。安装先以当前用户验证实际项目、VFS 状态目录、安装目录、项目父目录和 shell 配置的权限，实际测试执行、符号链接与 reflink 克隆。之后请求 `sudo` 完成运行依赖准备，必要时启用发行版提供的 AppArmor `bwrap` profile；实际沙箱命令通过后才安装私有 Go 工具链和 Threadmill。

**不提供普通复制降级。** 项目及默认 `~/.threadmill/projects/<项目路径哈希>/vfs` 必须允许 reflink 克隆，通常要求位于同一个启用 reflink 的 Btrfs/XFS 文件系统。仅有文件系统名称或 sudo 权限不算通过；ext4、跨文件系统或受限目录会在预检中拒绝。Threadmill、私有 Go 工具链和构建缓存放在 `~/.threadmill`，`~/.threadmill/bin` 幂等加入 shell 启动文件。安装器检查默认存储位置；使用 `vfs.live_root` 自定义位置或切换项目时，启动还会对实际路径重新准入。
```sh
curl -fsSL https://raw.githubusercontent.com/KDZZZZZZ/threadmill/dev-native/scripts/install.sh | sh
```

安装不会读取或写入模型密钥。打开新终端后，在通过准入的项目目录首次运行：

```sh
threadmill
```

TUI 会询问 API 地址、模型、上下文窗口和凭据名，并用无回显输入读取 API key。配置保存到 `~/.threadmill/config.yaml`，密钥单独保存到权限为 `0600` 的 `~/.threadmill/credentials.yaml`。

在源码仓库中开发时仍可直接安装当前工作区版本：

```sh
GOBIN="$HOME/.threadmill/bin" go install ./cmd/threadmill
```

## 打开 CLI

在满足文件系统和权限要求的项目目录运行即可进入 TUI：

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

## 任务、依赖与持久线程

每个 task 都有 Planner → Executor → Verifier 三个角色。角色之间和 task 之间都使用普通的 `from → to` 依赖，不设 root 类别，也不按创建顺序串行或隐式继承上一个 task。目标读取全部前驱的固定文件与记忆快照；共同部分直接取，先处理文件差异，再整理记忆差异。只有一个前驱时直接继承，不调用记忆整理。

task 可以没有向其他 task 的出边。没有任务需要它的输出时，它的运行或等待不会阻塞无关任务。持久 task 完成一轮后进入 `idle`，继续时保留 task ID，创建新的激活、环境和角色节点，并显式继承上一轮 verifier 输出；关闭只停止这个 task 的当前激活。多个 task 仍共享有限的模型和命令资源。

新阶段开始时，Manager 创建 `real_directory=true` 的新 task。真实目录现有内容作为额外来源进入 pending，按原有文件优先流程合入后，由该 task 直接在真实目录推进、调试和验收。运行中 Manager 与持有者可以双向交流；用户要求运行当前工作区时也创建新 task。已有 task 不能切换目录模式，其他 task 继续隔离。真实改动即时可见，失败不会自动回滚；文件可见、任务完成和 Verifier PASS 是不同的事实。项目在会话间发生变化时，新任务可使用新的只读 floor，旧快照继续引用原来的文件事实。归档在 reflink 后端可能保留磁盘文件；当前没有新增快照垃圾回收。

升级到统一边实现时，协调图和激活进度使用版本 `1`，会拒绝旧 root/Join 状态，**没有自动转换器**。切换前保留旧状态与匹配的旧程序；需要接续旧工作时，先用旧版完成，或从人工核对后的项目状态建立新图，不能把旧 `Finished`/`Merged` 标志当成新输入已就绪。接口、恢复边界与测试入口见 [统一边设计](docs/unified-edge-design.md)。

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

运行时还会注入 manager 的最新协调图、用户消息与 task 报告、受保护的 task package、上游输出与 Input 阶段信息，以及压缩所需的已有记忆和对话。每个 task package 只包含分配给它的 Task Info 和明确关联的用户请求；创建关系和继承记忆不扩大授权。文件差异处理使用独立会话，原始候选材料不会作为正常角色的当前记忆提前注入。当前模块与接口映射见 [统一边设计](docs/unified-edge-design.md)。修改提示词应在固定任务集上比较成功率、工具误用、证据完整性、token、延迟和费用，不能只凭文案判断。

开发验证同样需要支持 reflink 的测试卷；例如将 `TMPDIR` 指向该卷上的可写目录后运行 `go test ./...`。安装验收：`sh scripts/install_test.sh`；在支持 reflink 与 bwrap 的 Linux 上加 `THREADMILL_TEST_REAL_PROBES=1` 可运行真实文件系统／沙箱探针（下载、包管理与 Go 安装仍用测试替身）。
