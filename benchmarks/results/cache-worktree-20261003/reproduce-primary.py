#!/usr/bin/env python3
"""Verify the six primary archives and reproduce CSVs/tables with Python stdlib.

Usage: python3 reproduce-primary.py --output /tmp/threadmill-primary-reproduced
The output directory must be new. No archive member is extracted to disk.
CSV values are unrounded medians of the three completed per-run statistics;
the Markdown tables alone format values to three decimal places. Capacity
uses only original uninterrupted runs, excluding the supplemental repeat.
This verifies archive integrity and aggregation, not the full trace/terminal
audit performed separately by audit-primary.py against the frozen harness.
"""

import argparse
import csv
import hashlib
import json
from pathlib import Path, PurePosixPath
from statistics import median
import tarfile

FIXTURES = ("synthetic", "ipython")
BACKENDS = ("pi-worktree", "threadmill-bwrap", "threadmill-external")
DISPLAY = ("pi-worktree", "threadmill-external", "threadmill-bwrap")
LABELS = dict(zip(DISPLAY, ("Pi worktree", "TM external", "TM bwrap")))
WIDTHS = (64, 128, 192, 256, 384, 448, 500, 576)
INTERRUPTED = ("ipython", "threadmill-external", 576)
SUPPLEMENT = "continuation-20261003/ipython/threadmill-external-w576-r3.json"
LATENCIES = tuple(f"{op}_{p}_ms" for op in ("write", "fork", "collect") for p in ("p50", "p95"))


def require(ok, message):
    if not ok:
        raise ValueError(message)


def sha256(path):
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def safe_relative(name):
    path = PurePosixPath(name)
    require(not path.is_absolute() and ".." not in path.parts and str(path) == name,
            f"unsafe or noncanonical path: {name}")
    return path


def read_archives(source):
    manifest = json.loads((source / "primary-archives.json").read_text())
    require(len(manifest) == 6, "expected six archives")
    rows, proofs, seen = {}, [], set()
    for entry in manifest:
        path = source / safe_relative(entry["archive"])
        require(path.resolve().is_relative_to(source), "archive escapes source directory")
        require(path.stat().st_size == entry["bytes"] and sha256(path) == entry["sha256"],
                f"archive size/SHA mismatch: {path}")
        expected = {item["path"]: item for item in entry["files"]}
        require(len(expected) == len(entry["files"]) == 24, "expected 24 unique archive members")
        found = set()
        with tarfile.open(path, "r:gz") as archive:
            for member in archive:
                safe_relative(member.name)
                require(member.isfile() and member.name in expected and member.name not in seen,
                        f"unexpected, duplicate or non-regular member: {member.name}")
                wanted = expected[member.name]
                require(member.size == wanted["bytes"], f"member size mismatch: {member.name}")
                with archive.extractfile(member) as stream:
                    raw = stream.read()
                require(len(raw) == wanted["bytes"] and hashlib.sha256(raw).hexdigest() == wanted["sha256"],
                        f"member SHA mismatch: {member.name}")
                row = json.loads(raw)
                fixture = PurePosixPath(member.name).parts[-2]
                key = (fixture, row["backend"], row["agents"], row["repeat"])
                require(key not in rows, f"duplicate measurement: {key}")
                normal_name = f"{fixture}/{row['backend']}-w{row['agents']}-r{row['repeat']}.json"
                require(member.name == (SUPPLEMENT if key == (*INTERRUPTED, 3) else normal_name),
                        f"unexpected attempt path: {member.name}")
                require(row["formal"] is True and row["version"] == 1 and not row["serial"],
                        f"unexpected measurement mode: {member.name}")
                require(row["process_exit"] == row["errors"] == row["operation_errors"] == 0
                        and row["terminal_state_errors"] == {}, f"unsuccessful measurement: {member.name}")
                if member.name == SUPPLEMENT:
                    require(row["attempt"] == 2 and row["prior_interrupted_attempt"] == "interruption-20261003/observation.json",
                            "supplemental attempt provenance mismatch")
                # Retain only aggregate inputs: the largest member is parsed once.
                rows[key] = {field: row[field] for field in
                             ("wall_ns", "commands_per_second", "errors", "latency")}
                rows[key]["physical_disk"] = {field: row["physical_disk"][field] for field in
                                              ("peak_delta_bytes", "retained_delta_bytes")}
                seen.add(member.name)
                found.add(member.name)
        require(found == expected.keys(), f"missing members: {path}")
        proofs.append({"archive": entry["archive"], "sha256": entry["sha256"],
                       "bytes": entry["bytes"], "members_verified": len(found),
                       "raw_bytes": sum(item["bytes"] for item in entry["files"])})
    require(rows.keys() == {(f, b, w, r) for f in FIXTURES for b in BACKENDS for w in WIDTHS for r in (1, 2, 3)},
            "primary matrix must contain exactly 144 distinct measurements")
    return rows, proofs


def aggregate(rows):
    metrics, capacity = [], {}
    for fixture in FIXTURES:
        for backend in BACKENDS:
            limit = median(rows[fixture, backend, WIDTHS[0], r]["wall_ns"] for r in (1, 2, 3)) * 1.25
            eligible = []
            for width in WIDTHS:
                key = fixture, backend, width
                samples = [rows[*key, r] for r in (1, 2, 3)]
                item = dict(fixture=fixture, backend=backend, agents=width, completed_repeats=3,
                            interrupted_attempts=int(key == INTERRUPTED),
                            wall_seconds=median(row["wall_ns"] for row in samples) / 1e9,
                            commands_per_second=median(row["commands_per_second"] for row in samples))
                item.update({field: median(row["physical_disk"][field] for row in samples)
                             for field in ("peak_delta_bytes", "retained_delta_bytes")})
                item["errors"] = sum(row["errors"] for row in samples)
                for field in LATENCIES:
                    operation, percentile, _ = field.split("_")
                    item[field] = median(row["latency"][operation][percentile + "_ns"] for row in samples) / 1e6
                metrics.append(item)
                if key != INTERRUPTED and item["errors"] == 0 and median(row["wall_ns"] for row in samples) <= limit:
                    eligible.append(width)
            peak = max(eligible) if eligible else None
            capacity[fixture, backend] = dict(wall_ns_limit=limit, effective_peak=peak,
                                             stable_width=WIDTHS[WIDTHS.index(peak) - 1] if peak and WIDTHS.index(peak) else None)
    return metrics, capacity


def table_text(metrics, capacity):
    by_key = {(row["fixture"], row["backend"], row["agents"]): row for row in metrics}
    lines = ["### 容量判定", "", "| Fixture | Backend | wall 阈值（秒） | 有效峰值 | 稳定宽度 |",
             "| --- | --- | ---: | ---: | --- |"]
    for fixture in FIXTURES:
        for backend in DISPLAY:
            row = capacity[fixture, backend]
            lines.append(f"| {fixture} | {LABELS[backend]} | {row['wall_ns_limit'] / 1e9:.3f} | {row['effective_peak']} | {row['stable_width'] or '未确定'} |")
    lines += ["", "最低档已经是有效峰值，登记规则没有更低一档可认定为稳定宽度，因此没有可报告的稳定宽度倍数。", "",
              "![运行成本和物理空间](../benchmarks/results/cache-worktree-20261003/primary-runtime.svg)", "",
              "![文件写入 P95](../benchmarks/results/cache-worktree-20261003/write-latency.svg)", "",
              "两个图的坐标范围在同类指标内一致。写入图采用明确标注的对数坐标；图中各点为逐次统计量的中位数，不是置信区间。GB/MB 均采用十进制。"]
    for fixture in FIXTURES:
        lines += ["", f"### {fixture}：完整主矩阵", "", "wall 单位为秒；吞吐为完成的 bash commands/s。", "",
                  "| 宽度 | Pi wall | external wall | bwrap wall | Pi commands/s | external commands/s | bwrap commands/s |",
                  "| ---: | ---: | ---: | ---: | ---: | ---: | ---: |"]
        for width in WIDTHS:
            label = str(width) + ("*" if fixture == "ipython" and width == 576 else "")
            values = [by_key[fixture, backend, width][field] for field in ("wall_seconds", "commands_per_second") for backend in DISPLAY]
            lines.append("| " + label + " | " + " | ".join(f"{value:.3f}" for value in values) + " |")
        lines += ["", "空间为相对于各次 before 的物理增量；peak 为每 200 ms 采样的观测峰值，retained 为收集并释放后的保留状态。", "",
                  "| 宽度 | Pi peak GB | external peak GB | bwrap peak GB | Pi retained MB | external retained MB | bwrap retained MB |",
                  "| ---: | ---: | ---: | ---: | ---: | ---: | ---: |"]
        for width in WIDTHS:
            label = str(width) + ("*" if fixture == "ipython" and width == 576 else "")
            values = [by_key[fixture, backend, width][field] / scale for field, scale in
                      (("peak_delta_bytes", 1e9), ("retained_delta_bytes", 1e6)) for backend in DISPLAY]
            lines.append("| " + label + " | " + " | ".join(f"{value:.3f}" for value in values) + " |")
        lines += ["", "以下延迟均为毫秒；每次先计算 P50/P95，再对三次完成记录取中位数。", "",
                  "| 宽度 | Backend | write P50 | write P95 | fork P50 | fork P95 | collect P50 | collect P95 |",
                  "| ---: | --- | ---: | ---: | ---: | ---: | ---: | ---: |"]
        for width in WIDTHS:
            for backend in DISPLAY:
                label = LABELS[backend] + ("*" if (fixture, backend, width) == INTERRUPTED else "")
                row = by_key[fixture, backend, width]
                lines.append(f"| {width} | {label} | " + " | ".join(f"{row[field]:.3f}" for field in LATENCIES) + " |")
    lines += ["", "* IPython / TM external / 576 包含两次原始完成与一次补测；另有一次退出时间与原因未知的中断尝试。补测进入描述性中位数，不能替换中断尝试进入容量验收。"]
    return "\n".join(lines) + "\n"


def write_csv(path, rows):
    with path.open("w", newline="") as stream:
        writer = csv.DictWriter(stream, fieldnames=rows[0].keys())
        writer.writeheader()
        writer.writerows(rows)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", type=Path, default=Path(__file__).resolve().parent)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    source, output = args.source.resolve(), args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    rows, proofs = read_archives(source)
    metrics, capacity = aggregate(rows)
    write_csv(output / "primary-metrics.csv", metrics)
    for fixture in FIXTURES:
        for backend in BACKENDS:
            plot_rows = [dict(agents=row["agents"], wall_seconds=row["wall_seconds"],
                              peak_GB=row["peak_delta_bytes"] / 1e9, retained_MB=row["retained_delta_bytes"] / 1e6,
                              write_p95_ms=row["write_p95_ms"]) for row in metrics
                         if (row["fixture"], row["backend"]) == (fixture, backend)]
            write_csv(output / f"{fixture}-{backend}.csv", plot_rows)
    (output / "primary-tables.md").write_text(table_text(metrics, capacity))
    comparisons = {path.name: path.read_bytes() == (source / path.name).read_bytes() for path in sorted(output.glob("*.csv"))}
    evidence = dict(archives=proofs, members_verified=len(rows), csv_byte_matches=comparisons,
                    capacity=[dict(fixture=f, backend=b, **capacity[f, b]) for f in FIXTURES for b in DISPLAY],
                    interrupted_tier=dict(fixture="ipython", backend="threadmill-external", agents=576,
                                          uninterrupted_completed_repeats=2, supplemental_completed_repeats=1,
                                          interrupted_attempts=1, formal_capacity_eligible=False,
                                          termination_time_and_cause="unknown", descriptive_aggregation="three completed repeats"))
    (output / "verification.json").write_text(json.dumps(evidence, indent=2) + "\n")
    require(all(comparisons.values()), f"CSV disagreement: {comparisons}")
    print(json.dumps(dict(archives_verified=len(proofs), members_verified=len(rows), csv_byte_matches=comparisons)))


if __name__ == "__main__":
    main()
