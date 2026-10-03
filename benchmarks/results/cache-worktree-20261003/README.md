# Pi worktree 运行时评测数据

主报告：[`docs/threadmill-vs-pi-worktree-performance.md`](../../../docs/threadmill-vs-pi-worktree-performance.md)。主矩阵及缓存测量源码固定于 `b839b28747ae398f7badec9d2d0e51ae7e732c74`；报告分支的当前运行时版本不是这些数字的来源。

## 复算主矩阵

需要 Python 3.12，仅使用标准库，不运行 benchmark、不调用模型。输出目录必须不存在：

```sh
python3 benchmarks/results/cache-worktree-20261003/reproduce-primary.py \
  --output /tmp/threadmill-primary-reproduced
```

脚本核验六个归档及 144 个成员的长度与 SHA-256，检查矩阵与成功记录身份，然后输出 7 个 CSV、`primary-tables.md` 和 `verification.json`。它不会把 tar 成员解压到磁盘。CSV 与 checked-in 文件必须逐字节一致，否则非零退出。Markdown 表格只在显示时保留三位小数；CSV 保留未舍入的逐次中位数。

各次 P50/P95 使用原始 JSON 保存的统计量，离线脚本复算重复间的中位数，没有独立重算单次分位数。逐操作审计检查操作身份、计数和时长范围；全部原始操作时长可供进一步核对。

`primary-verification.json` 保存已执行验证的命令、摘要和结果。完整逐操作审计另由 `audit-primary.py` 检查原始轨迹、生命周期操作、退出码、终态、吞吐和物理空间算术；先完成下一节的解包，再提供对应冻结源码。脚本会先验证 helper 摘要：

```sh
python3 benchmarks/results/cache-worktree-20261003/audit-primary.py \
  --raw /tmp/threadmill-evidence/primary \
  --project /path/to/threadmill-at-b839b28 \
  --output /tmp/threadmill-primary-audit.json
```

`--raw` 必须保留清单中的相对目录结构。主矩阵共有 143 次原始完整回放与一次另存补测。IPython/external/576 的第三次原始尝试退出原因和时间未知；补测进入带限制的描述性统计，不能替换原尝试进入容量验收。两个复算入口都保留这一边界。

## 解包与其他矩阵审计

`primary-archives.json` 列出六个主数据包；`evidence-archives.json` 列出登记、参考组、缓存、历史组和诊断证据。先核验整包摘要，再分别解到 `primary`、`continuous`、`legacy`、`diagnostics`。不同轮次都使用 `synthetic/registration.json` 等名字，不能解到同一个根目录。下面的输出目录必须不存在：

```sh
python3 - <<'PY'
import hashlib, json, tarfile
from pathlib import Path

root = Path('benchmarks/results/cache-worktree-20261003')
entries = json.loads((root / 'primary-archives.json').read_text())
entries += [dict(archive=item['path'], sha256=item['sha256'], bytes=item['bytes'])
            for item in json.loads((root / 'evidence-archives.json').read_text())['archives']]
for item in entries:
    content = (root / item['archive']).read_bytes()
    if len(content) != item['bytes'] or hashlib.sha256(content).hexdigest() != item['sha256']:
        raise SystemExit('Archive differs: ' + item['archive'])
output = Path('/tmp/threadmill-evidence')
output.mkdir()
for item in entries:
    name = Path(item['archive']).name.removesuffix('.tar.gz')
    if name.startswith('primary-'):
        group = 'primary'
    elif name in {'continuous-control', 'cache-go-incomplete', 'cache-pytest',
                  'reference-synthetic', 'reference-ipython'}:
        group = 'continuous'
    elif name in {'legacy-metadata', 'legacy-synthetic', 'legacy-ipython'}:
        group = 'legacy'
    else:
        group = 'diagnostics'
    destination = output / group
    destination.mkdir(exist_ok=True)
    with tarfile.open(root / item['archive'], 'r:gz') as archive:
        archive.extractall(destination, filter='data')
print(output)
PY
```

解包不会执行其中的测量、诊断、AppArmor 或 Compose 文件。以下离线审计只读取保存的 JSON 和经摘要核验的冻结 Python helper，不运行工作负载：

19 个归档共 326,218,018 字节；按上述入口解包后，全部五个审计入口均已通过。解包摘要、命令和结果见 [publication-validation.json](publication-validation.json)。

```sh
python3 benchmarks/results/cache-worktree-20261003/audit-cache.py \
  --raw /tmp/threadmill-evidence/continuous/cache-pytest \
  --project /path/to/threadmill-at-b839b28 \
  --output /tmp/threadmill-pytest-audit.json
python3 benchmarks/results/cache-worktree-20261003/audit-reference.py \
  --raw /tmp/threadmill-evidence/continuous/synthetic-shared-cwd-reference \
  --primary-registration /tmp/threadmill-evidence/primary/preregistration.json \
  --project /path/to/threadmill-at-b839b28 --fixture synthetic \
  --output /tmp/threadmill-synthetic-reference-audit.json
python3 benchmarks/results/cache-worktree-20261003/audit-reference.py \
  --raw /tmp/threadmill-evidence/continuous/ipython-shared-cwd-reference \
  --primary-registration /tmp/threadmill-evidence/primary/preregistration.json \
  --project /path/to/threadmill-at-b839b28 --fixture ipython \
  --output /tmp/threadmill-ipython-reference-audit.json
python3 benchmarks/results/cache-worktree-20261003/audit-legacy.py \
  --raw /tmp/threadmill-evidence/legacy \
  --primary-raw /tmp/threadmill-evidence/primary \
  --project /path/to/threadmill-at-e177fa0 \
  --output /tmp/threadmill-legacy-audit
```

各输出文件或目录必须不存在。历史 helper 固定为 `e177fa04559275ee2a7604f4513a887df23c9c44`；主表、缓存和共享 cwd helper 固定为 `b839b28747ae398f7badec9d2d0e51ae7e732c74`。历史审计复核保存的完整 checkpoint restore 证据，不重新挂载或恢复 checkpoint。共享 cwd 没有文件隔离，不能进入隔离容量或磁盘节省结论。

参考组和历史组各有 48 条已审计记录。用本目录的审计汇总重建两组表格：

```sh
python3 benchmarks/results/cache-worktree-20261003/render-comparisons.py \
  --kind reference --output /tmp/threadmill-reference-tables
python3 benchmarks/results/cache-worktree-20261003/render-comparisons.py \
  --kind legacy --output /tmp/threadmill-legacy-tables
python3 benchmarks/results/cache-worktree-20261003/test_render_comparisons.py
```

每次生成 16 行 CSV、Markdown 表和输入摘要。容量失败如果缺少延迟统计，CSV 保留空单元格、表格显示“未记录”，错误数仍保留；不把缺失值写成零。CLI 回归及修复前后记录见 `render-validation.json`，历史审计的三个负向 CLI 检查见 `legacy-audit/validation.json`。

新的 Go 缓存阶段只有 2/40 行，旧 15 行有方法问题；`audit-cache.py` 会拒绝将不完整矩阵当作十次重复的正式结果。诊断归档仅包含白名单证据，不包含私有环境、模型配置、完整系统调用日志、二进制或整个构建缓存。提案文件中的“未执行”是生成时状态；后续实际检查以相应 root review、amendment 和 supervisor 记录为准，提案本身不是运行成功证明。

## 重绘图形

已有 CSV 即可重绘，不需解压原始 JSON。使用 Gnuplot 6.0，在本目录运行：

```sh
gnuplot primary.gnuplot
gnuplot write-latency.gnuplot
```

输出为独立静态 SVG。`gnuplot -e 'PNG=1' primary.gnuplot` 可生成审阅用 PNG。图中 GB/MB 使用十进制；峰值为 200 ms 采样观测值，写入 P95 图使用对数轴；各点都是每次统计量的三次中位数，不表示置信区间。

## 测量复现边界

重跑需要独立 Btrfs 文件系统、固定 Go/Python/Node/Pi 源码与离线构建缓存、冻结的 harness 和原始登记中的参数。见[事前协议](../../../docs/cache-worktree-benchmark-protocol.md)。用于分析的解包目录与用于重新测量的工作目录是两个用途。

正式回放串行。后续缓存矩阵使用持续回收已退出子进程的外层 supervisor；此前延后回收的 15 条 Go 记录属于方法诊断。缓存微基准的脚本 oracle 不等价于 Harbor 的 fresh 影子审计。

原始运行路径记载的是当时机器位置；复制本结果目录不会使那些路径自动存在。测量二进制、整个 npm 缓存、镜像和私人配置不嵌入结果包。SHA 能验证指定文件的一致性，不使不同宿主或后续依赖下载变成相同测量环境。
