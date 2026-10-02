# 命令缓存 Harbor 配对实验协议

本文在模型实验之前登记阶段 0 和阶段 5 的条件。阶段 3/4 的 Pi 对照为 **Pi 原生工具 + 每个 Agent 一个 git worktree**，由运行时 harness 单独测量；这一轮 Harbor 不增加 Pi 适配器。正式测量必须等缓存修复通过 PR 进入 `main`，固定在该提交上运行。开发期 pilot 必须标为 pilot，不能用于简历数字。

## 人类明确要求

- Docker data-root 位于 Btrfs，实际 storage driver 为 `btrfs`；容器仓库到 VFS 卷必须实测 reflink 成功。
- `THREADMILL_STRACE_BINARY` 上传静态 strace，版本至少 5.3。
- reflink、strace 版本和追踪能力检查失败时，在首次模型请求前退出。
- pilot 为 3 题各 1 次；正式实验为 10–15 题，各 5 次，按题配对。
- 0 组关闭缓存且样本较小；A 组每次命中都真实执行；B 组抽样率 0.01；C 组在 B 上增加缓存提示。
- A 出现退出码或写集不一致，停止并补依赖，不能进入 B。仅输出差异单独计数。
- 报告 P50/P90/P95 和置信区间，并关注通过率、token 和 bash 调用数。
- 运行时容量规则事前固定：有效峰值是中位 wall 不超过最低并发档 1.25 倍、每次均零错误的最高档；稳定宽度是它的前一档。首档即峰值时没有可报告的前一档。Go 与 Node 的 RSS 只作补充观察。

## Agent 选择的实现细节

复用现有 Harbor/Pier installed-agent 适配器，增加 `-exec-doctor` 预检和 `cache_mode=off|shadow|live` 参数。实验脚本仅使用标准库和 Harbor 已有的依赖；固定二进制、tracer、任务文件、镜像 ID、Harbor 版本和非秘密启动配置的哈希。正式样本须完整，不删除慢题、失败题或缺失配对。

默认 0 组使用输入列表的前 3 题；可以事前指定 1–3 题。题目顺序及重采样种子默认 `20261002`。模型的采样参数来自固定配置，脚本的种子不声称控制模型采样。C 组默认不运行；只有 A 的近似 miss 数据支持实际已实现的提示功能时，才提供 C 的提示配置。C 配置只允许增加 `exec.cache` 提示设置，并在 B 的基础配置上合并。

## 硬预检

静态 strace 6.8 的官方源码提交为 `a592648ae7d380b79ffb65275ec153b9e4844a57`，源码包 SHA256 为 `ba6950a96824cdf93a584fa04f0a733896d2a6bc5f0ad9ffe505d9b41e970149`。构建脚本把配置、编译日志及二进制哈希保留在指定目录，不安装主机软件：

```sh
benchmarks/harbor/build-static-strace /absolute/evaluation/strace-build
export THREADMILL_STRACE_BINARY=/absolute/evaluation/strace-build/strace
benchmarks/harbor/bench build
benchmarks/harbor/bench doctor --image <preloaded-task-image> > /absolute/evaluation/doctor.json
```

`doctor` 输出 JSON 并在任意条件失败时返回非零。它检查 `docker info` 的真实 driver 和 data-root 文件系统，在同一个 Docker daemon 中创建临时容器，以镜像中的目录作仓库来源、命名卷挂载 `/threadmill-vfs`，复用适配器安装检查。源文件包含 4 KiB 随机数据；`cp --reflink=always` 必须成功且 `cmp` 相等。空文件复制不算通过。探测容器只使用网络 `none`；其仓库与 VFS 挂载方式、`SYS_ADMIN` 和 AppArmor 设置与 Harbor 适配器一致。

安装前检查 tracer 的 ELF interpreter 和动态依赖，拒绝动态二进制。容器内检查版本，并实际跟踪一个文件读取。启动前写好配置，再以与正式运行相同的 HOME、TMPDIR、cwd、配置执行 `-exec-doctor`。只有 JSON 显式确认能力可用时才发出 `-p`；A/B/C 同时必须确认实际追踪已启用。0 组必须确认能力可用、实际追踪未启用。`exec.require_dependency_tracing: true` 保留在正常启动配置中，以防能力在预检后丢失。

Docker data-root 在 Btrfs 上并不意味着 driver 已是 `btrfs`。依据 [Docker Btrfs 文档](https://docs.docker.com/engine/storage/drivers/btrfs-driver/#configure-docker-to-use-the-btrfs-storage-driver)，两项分别检查。现有 daemon 的配置不由脚本修改；需要独立 daemon 时，通过 `DOCKER_HOST` 指向它，并同时隔离 containerd 的 socket 和 root。

## 配对运行

先准备本地任务文件和已构建/已下载的任务镜像。`prepare` 要求二进制、tracer、工作目录和 Harbor 版本与 doctor 记录一致；复用 Harbor 的环境内容标识定位镜像，并记录实际 ID。不能在实验中把镜像标签移动到另一个镜像。任务的隐藏测试和 grader 保持原样。provider/model 与基础配置由实验者明确指定，凭证继续使用 Harbor 的既有环境变量，不写进 plan。通过判据也须事前固定：默认 `--reward-key reward --pass-value 1`；SWERefactorBench 的已阅 scorer 使用 `score` 和 100 分上限，应指定 `--reward-key score --pass-value 100`。附加的数值来源字段或阶段计数不当作通过判据。

```sh
benchmarks/harbor/bench cache-ab prepare \
  --phase pilot --output /absolute/evaluation/harbor-pilot \
  --doctor /absolute/evaluation/doctor.json \
  --binary /absolute/evaluation/threadmill \
  --tracer "$THREADMILL_STRACE_BINARY" \
  --model <provider/model> \
  --task /absolute/dataset/task1 \
  --task /absolute/dataset/task2 \
  --task /absolute/dataset/task3

benchmarks/harbor/bench cache-ab run /absolute/evaluation/harbor-pilot --group off
benchmarks/harbor/bench cache-ab run /absolute/evaluation/harbor-pilot --group A
benchmarks/harbor/bench cache-ab audit /absolute/evaluation/harbor-pilot
benchmarks/harbor/bench cache-ab run /absolute/evaluation/harbor-pilot --group B
benchmarks/harbor/bench cache-ab report /absolute/evaluation/harbor-pilot
```

每次只启动一个 Harbor trial，固定 `--n-attempts 1` 和预登记 job 名，按 `(task, attempt)` 配对。完整 `run` 自动依次运行 0、A、B、可选 C。缓存设置放在最高优先级运行配置中：

| 组 | `exec.cache.enabled` | `exec.cache.verify_sample_rate` | 追踪状态 |
| --- | --- | --- | --- |
| 0 | false | 0.01（不生效） | 能力可用，未启用 |
| A | true | 1.0 | 启用 |
| B | true | 0.01 | 启用 |
| C | true | 0.01，另加提示配置 | 启用 |

正式实验使用新的输出目录，把 `--phase` 改为 `full`，登记 10–15 个 `--task`；脚本固定每题 5 次。源码必须处于 clean 的固定提交。模型、任务、二进制、配置、镜像、daemon、非秘密启动参数任一发生变化都拒绝继续。题目或结果缺失时 `report` 返回非零，仍保存已有观察。

基础配置可通过 `--config <yaml-or-json>` 指定。附加 Harbor 资源参数可以重复使用 `--harbor-arg=--override-cpus --harbor-arg=2` 等形式，它们随 plan 固定；不能覆写模型、任务、组、尝试次数，替换或禁用 grader。可选 `--hints-config <json>` 仅用于 C，文件形如 `{"exec":{"cache":{<实际已实现的提示选项>}}}`，并记录原文件和合并后的配置哈希。

## A 到 B 的门禁

每个 A trial 必须包含显式的 `exec_dependency_tracing`、`cmdcache_hits`、`cmdcache_verifications` 和三类 mismatch 计数。A 必须满足 `verifications == hits`，不能把缺字段当作零。任意退出码或写集 mismatch、trial 异常、缺失预检或审计数据立即停止 A。输出 mismatch 保留其总数，单独报告。

A 完成后，B/C 还要求全部 A trial 存在且至少有一次实际验证命中。零命中标为“审计证据不足”，不能写成“影子审计零误命中”。即使只运行 `--group B`，也会重新检查完整 A 数据。

## 可复算报告

原始 Harbor `result.json`、`setup.txt`、`preflight.json` 和 runtime snapshot 保留。每个 trial 另记录镜像前后 ID；`observations.json` 保存规范化配对行，`report.json` 保存计算结果和缺失项。

- wall：trial 的 `finished_at - started_at`，包含环境准备、适配器安装、执行、grader 和日志收集。
- agent 时间：Harbor 的 `agent_execution` 区间，包含相同的运行时预检与状态收集。
- 通过：选定的 grader reward key 达到事前固定的通过分值；缺少该 key 或非有限数值按证据缺失处理。同时保留全部原始 reward，不把部分分数或阶段计数冒充通过率。
- token：runtime 的模型输入/输出加记忆整理输入/输出。
- bash 调用数：当前以 `exec_requests` 记录执行请求，并在输出中明确标注来源；它不是单独的工具事件计数。

组内报告 mean 和 P50/P90/P95。配对差值为 `A - 0`、`B - A`、`C - B`；时间差值负数表示更快。以题目为 cluster，整题连同全部 5 次重复一起重采样，保留同次配对；默认 2000 次，使用 percentile 95% 区间，同时输出均值差、同次差值分布的 P50/P90/P95 及各自区间。差值分布的分位数不等同于两组分位数相减。pilot 的区间只用于检查计算流水线。正式区间描述所选题目及重复样本，不宣称证明普遍通过率等价。

事前预测：Go 内建测试缓存可能压低 cmdcache 的增益，pytest 的结果复用收益预计更明显；B 预计节省时间，且所选样本的通过率不下降。C 的 token、执行请求数或通过率退化必须与时间增益并列报告。追踪和入库的净收益分解由阶段 4 微基准报告，端到端报告展示配对总时间差。

## 来源与当前证据

Harbor 0.22.0 的 `BaseInstalledAgent._exec` 对非零返回码抛异常，适配器在该边界取证；公共入口用法参见 [Harbor custom agents](https://docs.harborframework.com/core-concepts/agents/custom-agents#run-a-custom-agent) 与 [run a job](https://docs.harborframework.com/core-concepts/jobs/run-a-job)。strace 源码与构建选项来自 [官方 v6.8 release](https://github.com/strace/strace/releases/tag/v6.8) 的 `README-configure` 和 `configure --help`。

横向参考只取相关能力边界：[Pi 的 bash operations](https://github.com/badlogic/pi-mono/blob/main/packages/coding-agent/src/core/tools/bash.ts) 默认在给定 cwd spawn；[deepseek-harness sandbox subsystem](https://github.com/deepseek-ai/deepseek-harness/blob/master/docs/subsystems/sandbox.md) 显式区分 backend enforcement 的 full/partial；[Eino filesystem backend](https://github.com/cloudwego/eino/blob/main/adk/filesystem/backend.go) 把执行能力交给 backend。Threadmill 沿用已批准的 Tool/Sandbox 边界，并在 Harbor 中实测能力；这些参考不代替 Threadmill 的硬条件。

2026-10-02 的现有 daemon 检查记录于 `/home/oops/evals/threadmill-cache-bench/doctor-existing.json`：实际 driver 为 `overlayfs`，data-root 文件系统为 Btrfs；静态 strace 6.8 通过；容器创建因 containerd metadata 无剩余空间而失败。因此该记录不能证明容器内 reflink 成功或失败，也不能支持缓存性能结论。后续独立 Btrfs daemon 的门禁结果应另存，不覆写这份观察。

同日独立 daemon 的通过记录为 `/home/oops/evals/threadmill-cache-bench/doctor-btrfs-ready.json`：driver 和 data-root 文件系统均为 Btrfs，容器内非空文件 reflink、mount namespace、静态 strace 6.8 的读取追踪均通过；`-exec-doctor` 确认所选 external/mount-namespace runtime 的追踪能力可用且已启用。由于任务镜像拉取超时、旧 daemon 的只读导出同样被满盘 metadata 阻断，该次使用本地预检 rootfs 镜像 `sha256:ffd84ec77971da756d66646888a93e0fd74fa082191a2131a0032e9ee5d633f3`。它证明此 daemon 与 runtime 的基础能力，不证明真实任务镜像已能完成安装；逐个任务仍必须通过相同门禁。尚未开始模型 pilot 或正式性能测量。

随后按照 [OCI image layout](https://github.com/opencontainers/image-spec/blob/main/image-layout.md) 从旧 containerd blob 目录只读导出真实 lang03 镜像，逐个核验 SHA256 和长度，附加 Docker 兼容的 `manifest.json`，由 [docker image load](https://docs.docker.com/reference/cli/docker/image/load/) 导入独立 daemon。镜像内容 ID 保持 `sha256:7c2ff0c6f15dde754fcd6958193a97d4f5d6336cb6000d6ccff85fb65facf458`；归档与核验记录为 `/home/oops/evals/threadmill-cache-bench/lang03-oci.tar` 和 `lang03-oci.json`。实际默认用户 `agent` 的记录 `/home/oops/evals/threadmill-cache-bench/doctor-btrfs-lang03-user.json` 显示安装时 root 探测全通过，但运行时因 `workspace_isolation_unavailable` 拒绝追踪。模型请求仍被挡住；需要解决监督进程的挂载能力与任务用户身份后，再验证真实任务就绪。

最终准入记录 `/home/oops/evals/threadmill-cache-bench/doctor-btrfs-lang03-admission.json` 另保存镜像 `User` 和实际 runtime 身份 `uid=2000(agent)`。只读检查 `lang03-user-capabilities.txt` 显示 sudo 不存在、inheritable/permitted/effective/ambient capabilities 均为零，已有 bash/setpriv/unshare 都是普通 0755 文件。当前没有现成受支持的挂载能力。Harbor 阶段 0 的真实任务外部前提因此未满足，pilot/A/B 保持停止；不通过切换 root 或新增特权包装程序解除门禁。复验须固定新的、已获批准的能力配置和镜像 ID，以原任务用户通过 doctor 和首次模型前的诊断，再登记 3 题各 1 次 pilot；正式实验还须满足 PR 进入 `main` 和完整样本条件。

最终源码重建的二进制 `/home/oops/evals/threadmill-cache-bench/threadmill-harbor-final` 的 SHA256 为 `fd5a2ef81550b579e3f54bdcf3750951317896e53e5ce119021729cda9613b3a`。同一二进制重验的 root 预检镜像通过记录为 `doctor-btrfs-root-final.json`，真实 lang03 用户失败记录为 `doctor-btrfs-lang03-final.json`；两者均在同一 evaluation 目录中，不能把前者用作真实任务准入证据。
