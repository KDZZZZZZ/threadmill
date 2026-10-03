# 98ed7d3 OverlayFS 历史组

状态：2026-10-03 已在独立普通 clone 构建旧运行时桥，在 synthetic 与 IPython 的专用只读 ext4 lower、Btrfs upper 上通过实际 native 回放、全部 checkpoint 恢复、缺失 upper 拒绝，以及 SIGTERM / SIGKILL 清理测试。共享采样器已验证 replay 原始 JSON 保留和采样后审计。正式 48 行尚未执行；必须等 PR 合入、构建与补登记冻结后，由唯一硬件 worker 串行运行。主矩阵 b839b28747ae398f7badec9d2d0e51ae7e732c74 的工作树和测量输入保持独立。

附件阶段 3 将 Threadmill `98ed7d3`（OverlayFS）列为必测历史组。桥源码是 `benchmarks/overlay-runtime/adapter/{main,trace,audit}.go.txt`，只在独立 98ed7d3 普通 clone 中生成 `cmd/tmlegacy/*.go`。main 不新增可链接新版 internal 却声称旧运行时的命令。桥通过旧版公开 VFS/Exec API 承接冻结 Trace v1；不修改旧运行时包，不回移新 Archive、reflink 策略、HOME 拆分、cmdcache、strace 或输出截断行为。

## 源码证据与存储条件

- 固定运行时基线：`98ed7d3d38c0fc956de0bc70c858f4ef65c90e02`。
- 已只读读取的 Git 对象：`98ed7d3:internal` = `7c119197cba0de95a940f6742f654b49b40d43ff`，`98ed7d3:go.mod` = `f1c18d82019e44e7e1325b04d5ee995214f9262b`，`98ed7d3:go.sum` = `d30ffee860e43ab46315b34632e3ab3b97509b27`。
- [旧 Store 构造条件](https://github.com/KDZZZZZZ/threadmill/blob/98ed7d3d38c0fc956de0bc70c858f4ef65c90e02/internal/vfs/store.go#L235)：只有 `Options.Overlay=true`、driver 可选择、且 `!ReflinkCloneable(baseDir, liveRoot)` 时才保留 Overlay driver。同设备 Btrfs 会走旧版的 reflink 优先分支，关闭 Overlay。
- [旧 ReflinkCloneable](https://github.com/KDZZZZZZ/threadmill/blob/98ed7d3d38c0fc956de0bc70c858f4ef65c90e02/internal/vfs/reflink.go#L27) 只组合文件系统类型和设备号；不能用新版能力探测替换这个历史选择器。
- [旧 driver 选择](https://github.com/KDZZZZZZ/threadmill/blob/98ed7d3d38c0fc956de0bc70c858f4ef65c90e02/internal/vfs/overlay_linux.go#L50)：EUID=0 选择 native，非 root 需 fuse-overlayfs 与 fusermount3。这个判断并不证明真实挂载成功。
- [旧 Handoff](https://github.com/KDZZZZZZ/threadmill/blob/98ed7d3d38c0fc956de0bc70c858f4ef65c90e02/internal/vfs/store.go#L388) 在父环境停止后搬迁 live、upper/work，关闭并重挂 native mount，继承已吸收的文件状态。
- [旧 Close](https://github.com/KDZZZZZZ/threadmill/blob/98ed7d3d38c0fc956de0bc70c858f4ef65c90e02/internal/vfs/store.go#L253) 只卸载，保留持久状态。持久 Store 的 [Release](https://github.com/KDZZZZZZ/threadmill/blob/98ed7d3d38c0fc956de0bc70c858f4ef65c90e02/internal/vfs/live.go#L290) 只 Absorb；Discard 才会删除持久目录，因此只能 Discard 原环境，不能 Discard retained checkpoint。
- [旧 Reap](https://github.com/KDZZZZZZ/threadmill/blob/98ed7d3d38c0fc956de0bc70c858f4ef65c90e02/internal/exec/scheduler.go#L489) 返回 kill/remove 错误，但没有新版 `runtime_cleanup_errors`。桥记录这些返回错误和残余运行时目录，不伪造旧版没有的指标。
- [旧 tmload](https://github.com/KDZZZZZZ/threadmill/blob/98ed7d3d38c0fc956de0bc70c858f4ef65c90e02/cmd/tmload/main.go) 没有 Trace v1 输入，生成分布式随机操作；它不足以直接满足逐条相同 trace 和完整 collect/release 的新实验口径。

采用专用 Btrfs 卷作为 `liveRoot` / upper / work，本组独立只读 ext4 loop filesystem 内的目录作为 immutable lower。每个 fixture 单独准备最小几百 MiB image（例如 256 MiB，须容纳其 archive 和文件系统元数据），用 `mkfs.ext4 -d` 将 archive 子目录写入 image，再将 loop 设备和 ext4 mount 都设为只读；注册前检查 kernel block readonly、loop backing、mount readonly 和 ext4 类型。不支持共享 host ext4 的只读 bind，避免其 whole-FS df 被宿主日志或其他进程改变。image backing 必须在 dedicated upper 卷外，本组日志/JSON 也不能写进该 ext4 loop filesystem。lower 必须由对应 fixture commit 的 `git archive` 生成；不能包含 `.git`、ext4 的 `lost+found` 或外部测试产物。这是为了触发旧代码已有 Overlay 条件的**历史存储布局**，与主矩阵同卷 Btrfs 布局不同，必须单列结果，不能从本组计算与主表的容量或磁盘节省倍数。lower 的 Git archive 内容，包括目录、普通文件、可执行位与符号链接，在独立审计中完整校验。若 fixture 的 export-ignore、gitlink 或特殊文件使该 archive 无法忠实回放被冻结的 trace，整组硬失败，不能删减 trace。

本组登记 native-overlayfs；保留旧默认 native 容量 1024，不设置 OverlayLimit。若实机身份只能走 fuse，不能静默改标签或提升旧默认容量 128；应先报告未满足本次登记条件。登记 EUID、capabilities、内核、两端 mount 信息。每个 agent 在 collect 前后以及 audit restore 时，都检查实际 live 的 statfs magic 为 native OverlayFS `0x794c7630`；`overlay_available=true` 仅是辅助证据。

## 回放与完整生命周期

回放输入必须是已经冻结的 stage-3 JSON 文件，不重新用旧随机生成器生成。要求 full fixture SHA、清洁 fixture repository 和 `gc.auto=0`；所有 fork/read/list/write/bash/think/collect/release 操作均记录耗时与错误，bash 还记录退出码、输出与输出摘要。

只接受当前 stage-3 生成器的只读 shell 命令：`true`、`ls . >/dev/null`、`find . -maxdepth 1 -type f | wc -l >/dev/null` 和固定小数 sleep。拒绝 `expected_cache` 与新 shell 命令，避免从文件工具 write 之外推导一个不完整的 checkpoint oracle。本桥不是阶段 4 缓存组。

1. setup：检查 JSON、fixture commit/clean state、文件系统类型和只读 mount；创建旧 persistent Store 与旧 external Scheduler。slots 与主登记相同，HeavyThreshold=24h，超时=120s，其余旧行为不改。setup 单列。
2. wall 开始后，每个 agent `Fork("", agent.ID)`；按 trace 执行操作。并行模式只并发 agent，单 agent 内保持原顺序；trace.serial=true 时串行。
3. collect：`Materialize(agent)` → 真实 Overlay 检查 → `Absorb(agent)` → `Handoff(agent, checkpoint)` → `Materialize(checkpoint)` → 真实 Overlay 检查。只读 agent 也必须获得 durable checkpoint；这些成本都计入 collect 和 wall。
4. release：对原 agent 调用 `Reap` → `Release` → `Discard`，全部计入 release 和 wall；保留 checkpoint。即使先前操作失败，也记录后续操作和清理错误。
5. 所有 agent 完成后，wall 内 `Store.Close()`，卸载 retained mounts，保留旧 upper/work。全局 Close 耗时另记 `final_close_ns`，不能将 release P50 当成全部卸载成本。
6. wall 停止后获取旧 Exec/VFS Stats 和运行时残余目录数。旧 `live_dirs` 可能仍包含保留的 mountpoint，不能把这个字段强行判为零；必须为零的是活跃命令、排队、进程组、runtime dirs、活跃 VFS I/O 与 Overlay mounts。

只读环境的 collect 会触发一次实际 Materialize，这是保留每个完成状态所必需的旧 API 操作；不添加新版 checkpoint API 来省掉该成本。旧 retained mounts 在最终 Close 之前继续占用旧 Overlay 槽位，这是旧行为，不能通过桥提前释放而改变历史容量。

## 独立 checkpoint 审计与接受条件

`-audit-only` 在 replay 进程结束、runner 完成 upper 的 peak/retained 样本保存后运行。审计耗时单列 `audit_ns`，不计入 replay wall、collect latency 或该行 upper peak。每行 replay 前不进行完整 lower 文件内容扫描，避免独有的预热；注册阶段生成 lower 与跨行 page cache 的影响要如实记录，不能称为冷缓存实验。

审计检查 replay 的 runtime/fixture/trace 身份、零错误和 checkpoint 数量，先确认旧 `.overlay-<sha256(id)>/{upper,work}` 与 mountpoint 实际存在，再使用另一实例的**原版** persistent Store 恢复。禁止让 Materialize 从 lower 新建一个缺失的 checkpoint 并假装恢复成功。

对每个恢复的 live 完整比较：对应 `git archive` 基线，加本 agent 按顺序产生的所有 write 内容；同时比较路径集合、文件类型、普通文件内容 SHA256、可执行位与符号链接目标。注册 stage-3 write 必须替换基线普通文件。该 oracle 检查所有 checkpoint，而非只抽样最后一个或只检查“有写过”。确认 native Overlay 后逐个 Close，保留旧 upper/work。审计完整性、卸载或文件读取任一失败都使该行无效。

最终 runner 接受一行必须同时满足：

- replay 进程退出 0、解析成功、`errors=0`、无总 error；每个 expected_exit 均满足。
- 旧 Exec 的 `queued/active/heavy_queued/heavy_active/tracked_process_groups/runtime_dirs` 全为 0。
- `adapter_reap_errors=0`、`adapter_orphan_runtime_dirs=0`、`adapter_runtime_dir_scan_errors=0`；后两者来自旧 scheduler 创建位置（liveRoot 的直接子项 `.threadmill-exec-*`）的实际目录扫描。扫描未执行/失败分别保留未观测值/错误，不能算作零。旧 VFS 的 `materialize_active/absorb_active/overlay_active` 全为 0。
- `final_close_errors=0` 且 `final_close_ns` 存在；完成路径只有 wall 内的那次 Close，返回错误归方法失败。setup 提前失败会清理 Store，保留未完成状态，不能补零冒充终态。
- `checkpoint_ids` 为全体 agent 的唯一对应集合，`checkpoint_overlay_proofs=agents`。
- 最终 Close 后逐个 statfs 独立核对，`checkpoint_close_proof_errors=0`；无法读取 statfs 也算证明失败。namespace 的自动销毁不能掩盖旧 Close 失败。
- audit 进程退出 0、JSON 完整、`passed=true`、`errors=0`；runtime/fixture/trace/replay-report 哈希全部匹配；`checkpoint_count=agents` 且审计 Overlay proofs=agents；审计终态活跃 VFS 数为 0。

runner 在读取审计后生成新的 final row：保存 replay 原 JSON 与 audit 原 JSON，不覆盖原始失败；final row 包含二者 SHA256、单列 audit_ns、`checkpoint_audit=passed|failed|missing`、失败列表与聚合 errors。缺失/损坏 audit、进程非零、缺少任何必需字段，至少产生一个非零 error，不能进入 capacity 候选。新版主矩阵的 terminal guard 不支持这些旧字段，需独立、严格的 legacy guard，不能降低主 guard 的要求。

## 正式构建、补登记和执行

桥和 runner 通过独立 PR 审查后，在普通 clone 中构建；不在已有主测量 worktree 或 Go1.24.2 stamping 有问题的 linked worktree 中构建。旧 runtime tree、go.mod、go.sum 三个对象必须仍与上列 98ed7d3 对象完全相等。任何改变都拒绝注册。

构建命令，所有路径由 runner 的新目录提供：

```sh
# 在基于 98ed7d3 的普通 clone 上只加入三个 cmd/tmlegacy 文件并提交 bridge patch。
git diff --exit-code 98ed7d3 HEAD -- internal go.mod go.sum
git status --porcelain
# 必须是已安装、固定的 Go 1.24.2；不在测量中下载/构建。
go build -mod=readonly -buildvcs=true -o /external/artifacts/tmlegacy ./cmd/tmlegacy
go version -m /external/artifacts/tmlegacy
```

构建身份 `vcs.revision` 为 bridge commit，`vcs.modified=false`；报告 runtime 基线单独为 full 98ed7d3。不能把 main 的二进制加一个 `runtime_commit` 常量当成历史运行时。补登记须记录 bridge commit、patch SHA256、三个旧 Git 对象、三个 adapter 文件哈希、binary SHA256、Go build metadata、legacy runner 源码 SHA256、协议/本设计哈希、系统/存储/身份信息。

登记矩阵：synthetic 3000×4KiB 与主矩阵选定真实 fixture；档位 `64,128,192,256,384,448,500,576`；各 3 次，共 **48 行**。使用相应主矩阵 fixture SHA 和每个 width 的原 trace SHA256；turns/seed/time-scale/command-duty、slots、宽度和容量规则都不改。只增加这一历史组，不重跑或替换主矩阵行；旧无效的方法调试行继续保留原样。48 行完成前登记文件与桥/runner 都冻结。

回放边界示例：

```sh
# sampler 只包住 replay 进程并保存 upper before/200ms sampled peak/after。
tmlegacy -trace-in /formal/trace-64.json -repo /fixtures/committed-repo \
  -lower /readonly-ext4/fixture -live-root /dedicated-btrfs/row/runtime \
  -slots 8 -json-out /results/legacy-w64-r1.replay.json
# sampler 已停止且 Btrfs after sync/du 样本已保存，才执行 audit。
tmlegacy -audit-only -trace-in /formal/trace-64.json -repo /fixtures/committed-repo \
  -lower /readonly-ext4/fixture -live-root /dedicated-btrfs/row/runtime \
  -replay-report /results/legacy-w64-r1.replay.json \
  -json-out /results/legacy-w64-r1.audit.json
```

物理占用沿用主 harness 的 `statvfs` before、200ms sampled peak、after，并在 before/after 做 Btrfs sync + raw filesystem du。采样范围是专用 Btrfs upper 卷，包含其文件数据与文件系统元数据；TMPDIR、所有 upper/work/live 和临时状态在该卷，日志/二进制/审计报告在卷外。lower 在每个 fixture 的独立只读 ext4 loop filesystem 上固定不变；另列 lower 的已分配文件 block 总量、这一个专用 ext4 的 before/after df 和原始口径，不能并入 Btrfs peak_delta 或推导 shared/exclusive。共享 lower 的固定空间不能被逐 agent 重复计数。登记 loop image 的 backing filesystem/文件已分配空间，image 不能放在专用 upper 采样卷；ext4 bitmap 使用量不等于 backing 文件系统实际 shared/exclusive 占用。本组独立 readonly loop 的 df/du/backing 前后必须相等；共享 host ext4 不受支持，其 df 变化不等于 source 内容变化。upper retained 样本在审计前保存，审计可能改变 work 状态或元数据，不能拿其后的空间覆盖原样本。

容量沿用先验规则（每个 fixture 的三次中位数 wall 不超过最低档 1.25 倍且全部零错误的最高档；稳定宽度为前一档），但历史布局单列表格，不能与主组作容量倍数结论。没有合格档位或最低档非零错误就报告 capacity 未成立，不跳过失败行。运行期间仍由唯一硬件 worker 串行测量。

执行前的公共 seam 验证：两 agent 的同一 Trace 回放；多次 write 覆盖正确且新 Store 恢复后逐树匹配；纯只读 checkpoint 保留；缺失 upper/work 或 fake full-copy 必须失败；Reap/Close 清理失败不能报告 errors=0；fixture/trace/report 哈希不一致失败；未知 shell/缓存 trace 失败。用真实 native Overlay 和只读 ext4/Btrfs 条件验证后才跑 48 行；能力不成立时报告具体失败，不绕开硬条件。

PR 的 Human Design 仅包括用户原附件明确要求的旧 `98ed7d3` Overlay 历史组、同 trace/fixture 和完整 collect/release 生命周期。Agent Self-Claimed 包括不改旧内部库的兼容桥选择、ext4 lower+Btrfs upper 布局、root 私有 namespace、`tmlegacy` 名称、CLI/schema、audit-only 实现、严格 shell 范围、观察字段、模板/构建方式与测试具体形状；这些是 agent 的实现决策，不能写成人类明确批准。这是基准兼容桥，不增加产品功能。

## 基准私有 namespace launcher

为了满足旧代码的 EUID=0 native 条件，整个固定 legacy runner 使用 root 身份，而非只让挂载子进程为 root。这样共享 sampler 的 after `btrfs du` 能递归读取旧版 0700 upper/work/live，不丢字段，不 chown 镜像。launcher 通过官方 `setpriv --pdeathsig KILL` 与以下结构化 argv 运行私有 namespace；不自写权限封装：

```text
unshare --mount --pid --fork --mount-proc --propagation private --kill-child=KILL -- <fixed-bench-entry> <frozen-row-arguments>
```

`fixed-bench-entry` 只接受已登记 binary/trace/fixture SHA 与预分配 upper/lower/output 路径；在 namespace 内将已只读的 ext4 lower 与 fixture/source 再分别作只读 bind，设置确定的 PATH，运行并等待固定 tmlegacy，再保存 namespace 销毁前的清理证明。所有本组新增 mount 均私有，不持久化 `/proc/*/ns/*`，不加入宿主或其他 daemon namespace。Btrfs upper 使用已由唯一 worker 准备的专用卷，历史测量不能修改其余主矩阵目录。已知 stage-3 命令不依赖网络；本组不增加网络隔离能力。不能临时追加任意 shell、`setns` 或 bwrap attachment。

依据 [util-linux v2.40 官方 unshare 文档](https://github.com/util-linux/util-linux/blob/v2.40/sys-utils/unshare.1.adoc#L69)，`--fork` 的等待父进程不转发 INT/TERM，故不能将“给 sudo/unshare 发 TERM”当成完整清理。root supervisor 应监控信号和超时，为 unshare 建独立进程组并设置 parent-death SIGKILL；收到 INT/TERM 时向已登记子组发送 SIGKILL、wait 到退出，再保存信号/进程退出证据。unshare 的 `--kill-child=KILL` 使 forked namespace init 在 unshare 死亡时被杀；结合 PID namespace 终止整个 namespace 子树。sup/worker 的突然退出路径必须验证，不能仅测试正常返回。

依据 [Linux 内核 private mount 语义](https://www.kernel.org/doc/html/latest/filesystems/sharedsubtree.html#detailed-semantics)，递归 private 阻止 mount/unmount 传播到其他 namespace。它不等于对所有宿主文件的隔离；仅允许上述已知只读 shell trace，fixture write 仍经旧 VFS jail 落入专用 upper。这是测量启动器的权限条件，作为 Agent Self-Claimed 单列；不会改变产品权限、Harbor workload UID 或给实际 coding agent 提权。

审计记录：supervisor/unshare/ns-init PID 与 namespace inode、固定进程树、实际 EUID/capabilities、lower 只读标志、namespace 中每个 Overlay 证明、Close 后的实际未挂载证明；正常/异常退出后无本组子进程或保留的 namespace FD、本组 mount 未出现在父 namespace。replay 与 audit 用两个相同配置的私有 namespace，audit 重新挂同一个 immutable lower，恢复同一个 persistent upper；核对 lower image/source 哈希和注册路径。持久 upper 数据允许留存，进程和挂载不允许留存。

已用真实进程验证 setpriv/unshare 的 Pdeath 与 signal 链、native mount、只读 ext4/Btrfs 兼容性、Go1.24.2 普通 clone 编译和 Handoff/Close 跨 Store 逐树恢复。信号测试先观察到实际 Overlay mount 与 bash 子进程，才发送信号；Linux 的 children 文件按线程暴露，测试观察 Go worker 的全部线程，并按 PID/starttime 等待退出。任一能力或证明不满足即阻塞补测，不能静默使用 fuse、reflink、普通复制或修改旧内部文件来补齐。

## 本次可审查补丁与公共 seam 测试

规范源码仅在 `benchmarks/overlay-runtime/`；`adapter/*.go.txt` 不参与产品 `go build ./...`。`bench.py prepare` 只接受已审查 project 中的 runner/templates，从固定旧 commit 建另一个普通 clone，在 detached HEAD 只提交三个生成文件，核验旧 internal/mod/sum Git 对象与模板 SHA256，禁用 GOWORK 和额外 GOFLAGS，构建真实 clean bridge，保存 main/template commit、runtime base、bridge commit、Go metadata 与 binary SHA。

现有 `benchmarks/pi-runtime/bench.py` 增加少量显式回调：默认 terminal guard/容量规则不改，增加 legacy 独立 guard 与 measured_output 参数以保留 Go 原始 JSON；中断路径在 finally kill/wait 本次子进程。legacy 复用原来的 physical_sample、measure、日志路径、dump、summarize 和登记 argv parser，不复制采样公式、1.25 容量规则或改变预测参数。成功行必须取得完整 audit；普通非零 command 容量错误保留继续；非 native、缺字段、Close/Reap/进程树/挂载证明失败与缺 audit 都保存 method failure 后停止新行。

补丁通过 PR 并冻结、唯一硬件 worker 可用后，正式操作入口如下：

```sh
python3 benchmarks/overlay-runtime/bench.py prepare --project /clean/main-source \
  --clone /new/ordinary-98-bridge --binary /artifacts/tmlegacy \
  --provenance /artifacts/tmlegacy-provenance.json --go /installed/go1.24.2/bin/go
python3 -m unittest discover -s benchmarks/overlay-runtime -p 'test_public_seams.py'
# 真实 native 公共 CLI 测试还需显式 TMLEGACY_SEAM_MANIFEST，不能把 skip 当 PASS。
sudo -n /usr/bin/setpriv --pdeathsig KILL python3 benchmarks/overlay-runtime/bench.py matrix \
  --project /clean/main-source --main-registration /formal/synthetic/registration.json \
  --trace-dir /formal/synthetic --fixture /fixtures/synthetic --lower /readonly-ext4/synthetic \
  --root /dedicated-btrfs/legacy-synthetic --output /results/legacy-synthetic \
  --binary /artifacts/tmlegacy --provenance /artifacts/tmlegacy-provenance.json \
  --dedicated-volume --dedicated-lower-loop
```

真实 fixture 对应另一次相同命令，复用它自身 main registration/trace，不重新生成操作；每次 24 行，两 fixture 共 48。matrix CLI 必须显式提供 `--dedicated-lower-loop`；runner 和 launcher 都核对实际 loop 设备 readonly 与 mount readonly，host ext4 readonly bind 会硬失败。lower 源须在 runner 外准备为独立只读 ext4 loop filesystem，replay/audit 再分别作只读 bind；从独立 namespace 复用同一路径与同一个冻结 binary/trace/fixture。lower 登记独立 ext4 df 和 `du --summarize --block-size=1` 的实际文件/目录分配块，后者不包含文件系统整体元数据。loop image backing 必须在 upper 卷外，另登记其文件已分配 blocks；完成后核对 lower df/du/backing 仍相同。

`test_public_seams.py` 只经 `verify-row`、生成 CLI 与 launcher 的公开入口观察：完整证据可通过且不造 runtime_cleanup_errors；缺 audit、缺 actual orphan/scan 计数、Close 返回错误或仍挂载、缺 after du 均不能 errors=0；普通 command 非零保留为 capacity failure。独立固定样例还覆盖 raw/measured 的 errors/error/serial/latency/throughput 一致性、不得增添或遗漏 trace 操作、必填非负整数 duration（拒绝 NaN/Infinity）、固定 before=10/peak=15/after=13 的 delta=5/3 算术，以及 fork 失败的既定 skip。只有 fork 失败才能省略该 agent 的 body 和 collect，fork/release 必须保留；所有其他操作键须与 trace 完全相等。显式 native 集成测试覆盖两个隔离 agent、连续写覆盖、只读 checkpoint、真实跨 Store 全树审计、删 upper 后拒绝伪恢复，以及 SIGTERM/SIGKILL 对 namespace 子树的清理。上述 native 和终止路径已在两套真实 fixture 上验证；测试结果不是正式容量测量。

性能证据还要求 setup_ns 为非负整数、commands_per_second 为有限非负数；latency 的类别及 count 必须与实际操作记录相符，p50_ns/p95_ns 必须为非负整数。新增固定 literal CLI 用例让 raw 与 measured 同时缺字段或带入 Infinity、负值、空/多余 latency 类别、错误 count，仍须拒绝。这里只校验类型、范围和类别/计数一致性，不重新实现分位数算法。这些 CLI 用例均已执行；总 error 与 errors 计数不一致的漏洞先由回归用例失败复现，再添加校验修复。

`TMLEGACY_SEAM_MANIFEST` 为执行前写在卷外的新 JSON：`binary`、`binary_sha256`、`fixture`、`fixture_commit`、`lower`、`scratch`、`regular_path`。lower 必须是本组独立只读 ext4 loop filesystem 内的 archive 目录；scratch 必须是专用 Btrfs 下不存在的新目录，regular_path 是该 fixture 中已知普通文件；集成测试需同样已有 root 启动条件。不提供该 manifest 时 native 测试会明确 skip，不能把 skip 算作实际 native 通过。

## 横向参考与边界

[Pi 7fbbd5f4 的 bash.ts](https://github.com/earendil-works/pi/blob/7fbbd5f4a1d982bb02d63472dde0774fa639f99b/packages/coding-agent/src/core/tools/bash.ts) 使用独立进程组、超时和退出码报告；同提交 `test/bash-close-hang-windows.test.ts` 检查后代持有 stdio 的退出行为。本桥保留旧 Exec 行为，在外层用 PID namespace 验证清理，不把 Pi 的本地 cwd 叫作隔离。DeepSeek Harness `c291e7961a515f6d7af9304e7fd1d257929aef26` 的本地 bash 实现和对应测试使用 managed subprocess range，并在观察到子进程准备后测试杀进程；Eino `9d983b36a5112a1c233056b1a099825298fafb8f` 的 filesystem 内存 backend 与读写/list 测试约束文件接口结果。后两者已核对本地固定版本摘录，完整上游文件路径未重新确认，故不虚构文件链接；它们都不能替代本组跨进程 durable checkpoint 的逐树 oracle。

Linux [OverlayFS 文档](https://docs.kernel.org/filesystems/overlayfs.html) 解释 upper/work 与 lower 的条件；实际选择及默认容量以上面固定旧源码为准。[setpriv v2.40 的 pdeathsig](https://github.com/util-linux/util-linux/blob/v2.40/sys-utils/setpriv.1.adoc) 与 [unshare 的 kill-child / fork](https://github.com/util-linux/util-linux/blob/v2.40/sys-utils/unshare.1.adoc) 是退出链的依据，正式登记同时保存已安装工具版本。
