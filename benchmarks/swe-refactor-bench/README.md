# SWE Refactor Bench

这里接入的是 2026-08-24 发布的 [SWE Refactor Bench][paper]，不是旧的
Java `SWE-Refactor` 数据集。官方仓库包含 20 道整仓迁移题：7 道语言重写、
7 道框架迁移、3 道平台移植和 3 道构建系统迁移。

## 固定版本

- 上游：[`Einsia/SWE-Refactor-Bench`][upstream]
- 提交：`1270471ccb5e6f254627784658d4e29acb90b953`
- 本接入验证过的 Harbor：`0.22.0`
- 本机要求：Python 3.12+、Docker、Git、Harbor；`pytest` 只在测试评分
  harness 自身时需要。

上游约 312 MiB，包含按各自许可证分发的原始项目归档。因此这里只固定
来源，不复制题库；`fetch` 将它放进被 Git 忽略的 `benchmarks/.work/`。

```sh
benchmarks/swe-refactor-bench/bench doctor
benchmarks/swe-refactor-bench/bench fetch
benchmarks/swe-refactor-bench/bench list
benchmarks/swe-refactor-bench/bench validate build01-libsodium-autotools-to-cmake
```

要把工作数据放到其他磁盘：

```sh
THREADMILL_BENCHMARK_HOME=/data/threadmill-benchmarks \
  benchmarks/swe-refactor-bench/bench fetch
```

## 官方评分链路

Agent 只看到 `environment/` 构建出的 `/workspace/repo` 和
`instruction.md`。隐藏测试位于单独 verifier 镜像中，Agent 容器销毁后，
Harbor 才把提交的源码重新物化进去。构建产物不会沿用。

Threadmill 运行时会为每条命令创建独立 mount/PID namespace，把当前
Agent 的 VFS live 映射到 `/workspace/repo`。通用 Harbor 脚本自动附加只含
`SYS_ADMIN` 和 AppArmor mount 放行的 Compose overlay；命令启动前会丢弃
`SYS_ADMIN`，因此并发 helper 中的绝对路径与 read/write/edit 看到同一份工作树。

总分只有 `0 / 40 / 50 / 60 / 70 / 80 / 90 / 100`：

1. **Migration Audit**：模型对照原仓库检查迁移是否真实完成；任一必需 gate
   失败，总分为 0。每个 gate 由 3 次采样投票。
2. **Behavioural**：固定模块化测试全部通过才得 40 分，部分通过不计分。
3. **Agentic Verification**：只有前两阶段完整通过才启动 6 个独立对抗 Agent；
   每个最多 1 小时，每个未找到可三次复现的差异得 10 分。

正式的官方基线命令使用上游固定的 Codex CLI 0.146.0：

```sh
jobs="$(benchmarks/swe-refactor-bench/bench jobs)"
suite="$(benchmarks/swe-refactor-bench/bench path)"
cd "$suite"
PYTHONPATH=infra \
SRB_CODEX_BINARY=/path/to/codex-0.146.0 \
OPENAI_API_KEY="$OPENAI_API_KEY" \
harbor run \
  -p tasks/build01-libsodium-autotools-to-cmake \
  -a swerefactor.harbor_agent:SrbCodex \
  -m gpt-5.6-sol \
  -ak version=0.146.0 \
  -ak reasoning_effort=ultra \
  -o "$jobs"
```

没有 cgroup CPU 或 memory controller 的内核，按官方说明在命令尾加
`--cpus ignore --memory ignore`。Harbor 默认把每次 trial 的 `result.json`、
`agent/trajectory.json`、`verifier/reward.json`、`verifier/score.json`、
`verifier/summary.txt` 和提交 artifact 放在 `-o` 指定的 job 目录中。

最短的 Agent 预算也是 6 小时，最长 30 小时；Verifier 还可能启动 6 个
一小时的对抗回合。因此 `validate` 只是离线校验题目配置，不代表跑过题目。
第一道端到端 smoke 建议用 `build01-libsodium-autotools-to-cmake`：它处于
最短预算档，且论文数据里构建系统迁移明显比语言重写更容易。这只是节省首轮
成本的选择，不改变评分协议。

## Threadmill 怎么接

不能把 Threadmill 直接跑在解压后的源码上再手工执行公开测试；那会破坏隐藏
测试边界，也拿不到三阶段正式分数。正确接入点是 Harbor 的自定义 Agent
adapter，职责只有：

1. 把固定提交构建出的 Threadmill 二进制和一次性配置上传到 Agent 容器；
2. 在 `/workspace/repo` 中无交互执行 `threadmill -p`；
3. 让 Harbor 继续负责容器、时间预算、gateway-only 网络和 artifact 收集；
4. 把 Threadmill 的协调图、记忆图、事件日志、token、缓存和性能快照收进
   trial 的 `agent/` 目录。

上游发布协议只接受 `codex-cli 0.146.0` 或 `claude-code 2.1.220` 作为固定
submission harness。换成 Threadmill 后，官方 verifier 仍能产生完全相同格式
的分数，但这是 **SWE Refactor grader 上的 Threadmill 实验分**，不能冒充论文
排行榜中同模型、固定 client 的可比成绩。报告必须同时记录上游提交、Harbor
版本、Threadmill 提交、模型、配置、时间预算和 adapter 版本。

通用适配器位于 `benchmarks/harbor/threadmill_agent.py`，继承 Harbor
`BaseInstalledAgent`：它上传当前构建的 Threadmill、写入一次性模型配置、在
`/workspace/repo` 运行，并把日志、checkpoint、协调/记忆图和 VFS 恢复状态收进
trial artifact。适配器单元测试与本目录 `install-only` smoke 已验证；正式分数仍
必须由本目录 `run` 触发完整三阶段 grader 得出。

[paper]: https://arxiv.org/abs/2608.23564
[upstream]: https://github.com/Einsia/SWE-Refactor-Bench
