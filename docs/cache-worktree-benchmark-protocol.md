# Threadmill / Pi + worktree 可复现性能协议

本协议在正式测量前登记。旧报告 [threadmill-vs-pi-architecture-and-performance.md](threadmill-vs-pi-architecture-and-performance.md) 保留为历史材料；新数字只来自本协议冻结后的新 commit 与原始 JSON。

## Human Design

- 主对照为 Pi 原生工具 + 每个 Agent 一个 git worktree；双方隔离，同一已提交 fixture，同一逐条 JSON trace，不调用模型。
- Threadmill 同时测 external 与 bwrap；fork、文件读写/列举、bash、收集增量、释放全部计时。3000 × 4 KiB 合成仓和一个 benchmark 题目的真实中型仓库。
- 并发 64、128、192、256、384、448、500、576，每档 3 次中位数；测物理 Btrfs 空间和延迟。
- 缓存测 Go `test/crash-recovery-project` 与 pytest；相同命令脚本对比开缓存、关缓存、Pi worktree，追踪开关各 10 次。

## Agent Self-Claimed

- trace v1、CLI 名称、串行缓存 oracle、Pi 源码 pin、Python 标准库编排、checkpoint 保存方式与具体测试形状。
- 使用上游完整 `npm run build:offline`；首次 hydrate 的模型目录与 npm 下载内容保存在仓库外缓存，记录文件摘要。
- 正式测量要求阶段 1 经 PR 合入 `origin/main` 后的干净 Threadmill commit、与它完全匹配的二进制、专用 Btrfs 文件系统、事前登记文件摘要；smoke 标为 `formal: false`。

## 事前预测与判定规则

以下是待证伪预测，不是实测结论；在任何正式矩阵之前冻结。

| 项目 | 事前预测 |
| --- | --- |
| 合成仓有效峰值 | Threadmill external 至少 384，bwrap 至少 256；Pi worktree 128–256 |
| fork / 写入 | 逻辑分叉的 P95 低于 worktree checkout；物化与 Archive 成本可能抵消 wall 优势，必须完整计入 |
| 真实中型仓 | checkout 随文件数增加；Threadmill 的 retained checkpoint 与 floor 元数据也增长，不能用逻辑字节推算物理节省 |
| Go 缓存 | Pi 的共享 GOCACHE 可使 `go test` 自带 `(cached)` 成为强对照，Threadmill 开缓存不保证更快 |
| pytest 缓存 | 确定性重跑的净收益预计为正；追踪与入库成本可能使短命令净收益为负 |

每个 fixture、backend 单独判定：最低并发档的 3 次 wall 中位数 × 1.25 为阈值；全部 3 次零错误且 wall 中位数不超过阈值的最高档为“有效峰值”；“稳定宽度”为它的前一档。若最低档已无合格结果，报告无有效峰值；若仅最低档合格，稳定宽度为未确定。原始失败不能删除或替换。文件写、fork、collect 的每次 P50/P95 再取 3 次中位数。三个重复不足时不作正式容量结论。

## 固定输入和 trace

`go build -o /path/to/tmload ./cmd/tmload`。导出不执行负载；回放不重新采样。每档导出一份 trace，三个重复和各 backend 共用该文件，报告原始文件 SHA-256。

```sh
python3 benchmarks/pi-runtime/bench.py fixture --repo /dedicated/fixtures/synthetic
/path/to/tmload -trace-out /results/trace-64.json -repo /dedicated/fixtures/synthetic -agents 64 -turns 40 -seed 42
/path/to/tmload -trace-in /results/trace-64.json -repo /dedicated/fixtures/synthetic -workdir /dedicated/run -sandbox external -json-out /results/tm.json
node benchmarks/pi-runtime/replay.mjs --pi-source /cache/pi --trace-in /results/trace-64.json --repo /dedicated/fixtures/synthetic --workdir /dedicated/worktrees --json-out /results/pi.json
```

JSON v1：`version`、`seed`、`fixture {kind,files,file_bytes,commit}`、可选 `serial`、`agents [{id,operations}]`。操作为 `think {duration_ns}`、`read/list {path}`、`write {path,content}`、`bash {command,expected_exit?,expected_cache?}`。内容是 UTF-8；读/写路径不允许越界或进入 `.git`。`expected_exit` 默认 0。每个 Agent 强制 fork → operations → collect → release；这些阶段不能靠 trace 省略。缓存微基准 `serial: true` 让第一次生产与之后复用的顺序可计算。

双方验证 `fixture.commit == git rev-parse HEAD` 且 fixture clean。Threadmill 以该 commit 的 `git archive` 作为 content-only floor（明确排除 `.git`），Pi checkout 同一树，保留其固有 `.git` 指针；trace 不访问 Git 元数据。所有 Git 操作 `gc.auto=0`，这是对 Pi 有利的去噪设置。

Threadmill collect 显式执行 `Absorb + Archive`，保存 `checkpoint-<agent-id>`；release 执行调度器 Reap、Release、Discard 原工作环境。持久 reflink checkpoint 保留到测量结束。Pi collect 为 `git add -A && git commit --allow-empty`，branch 与 commit 对象保留到测量结束；release 为 `git worktree remove --force`。Archive 不隐藏在 setup 或 cleanup 中。`setup_ns` 单列源码读取、floor 建立/原生工具导入，`wall_ns` 为所有 Agent 完整生命周期。

真实仓准备入口：`fixture --source-repo /benchmark/task/repo --commit <full-sha1>`。选择 DeepSWE `ipython-session-bundle-replay` 的 [ipython/ipython@0bb317d10fdcb3aa13beb1031d5f10e5b821203b](https://github.com/ipython/ipython/tree/0bb317d10fdcb3aa13beb1031d5f10e5b821203b)，源于 dataset `435ee89ec2f2e2289f33b0da4f992f0b7b7266b9` 的该题 `environment/Dockerfile`；473 个 tracked files、7,481,778 个 regular file bytes，来源登记在 [fixtures.json](../benchmarks/pi-runtime/fixtures.json)。这是真实中型代码库，文件数少于合成仓，结果不用于证明“文件数越多越有利”。选题、具体来源、文件数、字节数、fixture SHA-1 必须在登记 JSON 中记录，正式矩阵不能用未说明来源的宿主工作区替代。Git LFS/submodule 的物化内容若不在 commit 树中，需要先形成独立已提交 fixture 并记录来源。

## Pi pin 和离线构建

[pi-pin.json](../benchmarks/pi-runtime/pi-pin.json) 固定 `earendil-works/pi@7fbbd5f4a1d982bb02d63472dde0774fa639f99b`，不是旧 `badlogic/pi-mono` 构建残留。Node ≥ 22.19.0。`prepare-pi --source /cache/pi --cache /cache/pi-build` 用固定 package-lock 下载依赖，首次执行上游 `hydrate:model-data`，缓存生成的 provider JSON，再执行完整 `build:offline`；`--offline` 禁止依赖下载与目录 hydrate，缺少缓存即失败。`pi-build.json` 保存 Node/npm、lockfile 摘要、全部模型目录文件摘要和构建方式，日志为 `prepare-pi.log`。模型目录仅用于完成真实上游构建，回放没有 provider 调用。

原生文件能力已按 pin 核对：[`tools/index.ts`](https://github.com/earendil-works/pi/blob/7fbbd5f4a1d982bb02d63472dde0774fa639f99b/packages/coding-agent/src/core/tools/index.ts) 导出 read/write/ls/bash。harness 调用 `create*ToolDefinition(cwd)`，使用默认操作与截断参数；[`ls.ts`](https://github.com/earendil-works/pi/blob/7fbbd5f4a1d982bb02d63472dde0774fa639f99b/packages/coding-agent/src/core/tools/ls.ts) 默认最多 500 项，原生 ls 是只读工具集的一部分，并非默认四个 coding 工具之一。[`bash.ts`](https://github.com/earendil-works/pi/blob/7fbbd5f4a1d982bb02d63472dde0774fa639f99b/packages/coding-agent/src/core/tools/bash.ts) 返回结构化退出码；不以文本猜测退出状态。不注入沙箱，不替换原生 FS 或 spawn。

横向参考只用于边界核对：deepseek-harness [`c291e796 docs/subsystems/sandbox.md`](https://github.com/deepseek-ai/deepseek-harness/blob/c291e796/docs/subsystems/sandbox.md) 明确 filesystem policy 与网络/进程可见性不同，不从无沙箱速度推导生产隔离能力；Eino [`9d983b36 adk/filesystem/backend.go`](https://github.com/cloudwego/eino/blob/9d983b36/adk/filesystem/backend.go) 将 Read/Write/List 与 Execute 的 backend 能力分开，这里沿用本项目已有 View/Scheduler seam，无新增模块依赖。

## 矩阵与物理空间

先 `bench.py doctor --root /dedicated/round --bwrap`，实测非空 `cp --reflink=always` 和 bwrap 命令成功。Docker/Harbor 另跑其 `bench doctor`；本运行时矩阵不使用 Docker，Docker data-root 正常不代表 Harbor repo→VFS 可 reflink。

```sh
python3 benchmarks/pi-runtime/bench.py matrix \
  --root /dedicated/round --dedicated-volume --output /results/round \
  --fixture /dedicated/fixtures/synthetic --tmload /path/to/tmload --pi-source /cache/pi
```

`--widths`、`--repeats`、`--turns`、`--seed`、`--time-scale`、`--command-duty`、`--slots` 可用于 smoke；正式 defaults 为上述矩阵、40 turns、seed 42、time-scale 0.05、command-duty 0.12、8 slots。组默认 bwrap/external/Pi worktree；`--groups` 可以加 `pi-shared-cwd`，它没有隔离，写入可相互覆盖，只是速度参考。每次重复旋转组顺序，所有 benchmark 进程串行跑，避免争用 CPU。

可选旧 `98ed7d3` OverlayFS 组不混入主结论。旧 tmload 不支持 JSON trace；只有将本 harness 接到旧 VFS 并验证完全相同操作、记录 harness patch 与旧源码 commit 后，才可作为历史衔接组。缺少这个适配时明确标“未测”，不能把旧报告中的同分布数字当作新 trace 数字。

物理空间的主证据为专用 Btrfs 文件系统 `df` 对应的 `statvfs` allocated bytes 前、采样 peak、收集与释放后。前后 `btrfs filesystem sync`，运行中每 200 ms 采样，不在每个 poll 强制 sync；sampled peak 是观测下界。保存所有样本以及 `btrfs filesystem du -s --raw` 的 shared/exclusive 原始输出。`du` apparent bytes 与公式不替代 df。fixture 的初始空间在 before 已存在；peak/retained 是其后的增量，涵盖 floor、live、checkpoint、Git 新对象和文件系统元数据。每次测量结束保存 retained 空间，随后删该次独立 runroot。

宿主 Docker 文件系统有并发写入时不具备物理空间归因条件。正式测量必须另挂专用 Btrfs image/卷，并记录 mount/device、空闲容量、内核、CPU、二进制 SHA-256 和 commit。共享现有 Btrfs 卷上的验证仅为 smoke，`formal: false`。RSS 只作为 Go/Node 运行时的补充观察，不用来证明 Agent OS 容量倍数。

## 缓存微基准

`benchmarks/cache-runtime/bench.py` 准备两个已提交 fixture，并生成同一个脚本 trace：Go 从 `test/crash-recovery-project` 保留测试、在 fixture 副本修复已知边界以获得绿色基线；pytest 使用 checked-in fixture。固定种子按概率改 pricing，再依次全量测试、单包测试与 Go vet，每条测试重跑一次；串行 Agent 的前序状态集合给出 `expected_cache`。首次某内容状态/命令为 miss，之后为语义上可复用；安全拒绝与 replay false hit 分开记录，不能把 miss 伪造为命中。

三组为 Threadmill cache on / cache off（均追踪）/ Pi worktree。Pi 组所有 worktree 共享同一 GOCACHE，保存原生 `(cached)` 次数；Threadmill 使用当前生产 per-environment HOME/GOCACHE。每组每次重复开始均为冷缓存，新工作根、同一已提交内容与固定 trace。pytest 禁用 `.pytest_cache` 与字节码写出（[pytest cacheprovider 说明](https://docs.pytest.org/en/stable/how-to/cache.html)），避免把 Python 工具本身的缓存当作命令结果缓存。

Python 依赖只装入独立评测 venv；[requirements-pytest.txt](../benchmarks/cache-runtime/requirements-pytest.txt) 固定 pytest 及其依赖。`--python` 的绝对路径写入同一 trace，bwrap 评测需让该 venv 位于已有 `/usr` 只读挂载内（本机为 `/usr/local/share/threadmill-bench/pytest-venv/bin/python`），不能为了测试放宽生产挂载。Go 同理用 `--go /usr/local/share/threadmill-bench/go1.24.2/bin/go`，脚本显式 `GOTOOLCHAIN=local`，避免一边自动下载新工具链、一边只用旧系统 Go。

```sh
python3 benchmarks/cache-runtime/bench.py --language go \
  --go /usr/local/share/threadmill-bench/go1.24.2/bin/go \
  --root /dedicated/cache-go --output /results/cache-go \
  --tmload /path/to/tmload --pi-source /cache/pi
python3 benchmarks/cache-runtime/bench.py --language pytest \
  --python /usr/local/share/threadmill-bench/pytest-venv/bin/python \
  --root /dedicated/cache-pytest --output /results/cache-pytest \
  --tmload /path/to/tmload --pi-source /cache/pi
```

追踪税单独以 cache off 比较 dependency tracing on/off，每种 10 个独立完整 trace。每个重复固定选择第一个 Agent 的第一条全量测试命令（当前 `agent-0`、operation index 1），从其 `operation.duration_ns` 提取一个样本，开/关各 10 个，导出 `BenchmarkCommand/<language>` 行；完整 trace wall 同时导出 `BenchmarkReplay/<language>` 行，用 `benchstat off.txt on.txt` 一并比较。登记 JSON 写明原始命令、Agent/index 与固定采样规则。单命令时长包括这次 `Exec.Run` 的物化、调度、执行追踪和 Absorb 边界；它不是裸子进程时长。所有样本来自已有回放，不额外运行命令。

缓存 wall 对比包括 lookup/入库/replay；快照包含 `saved_duration`、`store_duration`、lookup 耗时。净收益主证据为相同 trace 的 cache-off/untraced wall − cache-on/traced wall；分解同时报告时间节省 − 实测完整 trace 追踪税 − 入库耗时，并注明跨进程 warm cache/等待/重叠造成的不可精确相加项。负值照实保留。

正式缓存测量要求 PATH 中存在 benchstat，登记其绝对路径和 SHA-256。本机工具固定为 `golang.org/x/perf/cmd/benchstat@v0.0.0-20250813145418-2f7363a06fe1`（上游 commit `2f7363a06fe1e84314f47158b693ef982b0c2255`），位于 `/home/oops/.cache/threadmill-bench/bin/benchstat`；正式命令前设置 `PATH=/home/oops/.cache/threadmill-bench/bin:$PATH`。每轮 `registration.json` 在回放前冻结 trace、protocol、Threadmill 二进制、Pi 构建和 harness 摘要以及实际 Python/Go/strace 版本。

本协议不承诺尚未跑出的容量数字、物理节省比例或零误命中；新性能报告只有在正式原始数据齐全后写。Harbor A 组是后续门禁，不能用无模型 smoke 推导通过率或“影子审计零误命中”。
