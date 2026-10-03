#!/usr/bin/env python3
"""Render reference or historical tables from completed offline audit summaries."""
import argparse
import csv
import hashlib
import json
from pathlib import Path

ROOT = Path(__file__).resolve().parent
WIDTHS = [64, 128, 192, 256, 384, 448, 500, 576]
FIELDS = ['fixture', 'backend', 'agents', 'repeats', 'wall_ns_median', 'errors',
          'commands_per_second_median', 'physical_peak_delta_bytes_median',
          'physical_retained_delta_bytes_median', 'write_p50_ns_median', 'write_p95_ns_median',
          'fork_p50_ns_median', 'fork_p95_ns_median', 'collect_p50_ns_median', 'collect_p95_ns_median']
cli = argparse.ArgumentParser(description=__doc__)
cli.add_argument('--kind', choices=['reference', 'legacy'], required=True)
cli.add_argument('--output', type=Path, required=True)
args = cli.parse_args()
inputs, groups = {}, {}
for fixture in ['synthetic', 'ipython']:
    path = ROOT / (f'{fixture}-reference-audit.json' if args.kind == 'reference' else 'legacy-audit/summary.json')
    content = path.read_bytes()
    audit = json.loads(content)
    inputs[str(path.relative_to(ROOT))] = hashlib.sha256(content).hexdigest()
    if args.kind == 'reference':
        if audit['fixture'] != fixture or audit['rows_verified'] != 24 or audit['errors'] != 0 or audit['reference_only'] is not True:
            raise SystemExit('Incomplete reference audit')
        groups[fixture] = audit['summary']
    else:
        if audit['rows_verified'] != 48 or audit['historical_only'] is not True:
            raise SystemExit('Incomplete historical audit')
        groups[fixture] = audit['summaries'][fixture]['capacity']

rows, markdown = [], []
for fixture, backends in groups.items():
    if len(backends) != 1:
        raise SystemExit('Expected one separate comparison backend')
    backend, group = next(iter(backends.items()))
    expected_backend = 'pi-shared-cwd' if args.kind == 'reference' else 'threadmill-historical98-overlay-external'
    if backend != expected_backend:
        raise SystemExit('Unexpected comparison backend')
    if [tier['agents'] for tier in group['tiers']] != WIDTHS or any(tier['repeats'] != 3 for tier in group['tiers']):
        raise SystemExit('Incomplete tier matrix')
    markdown += [f'### {fixture}', '',
                 '| 宽度 | wall 秒 | commands/s | write P95 ms | peak 增量 MB | retained 增量 MB | 错误数 |',
                 '| ---: | ---: | ---: | ---: | ---: | ---: | ---: |']
    for tier in group['tiers']:
        rows.append(dict(fixture=fixture, backend=backend, **tier))
        write_p95 = tier.get('write_p95_ns_median')
        values = [tier['agents'], f"{tier['wall_ns_median'] / 1e9:.3f}",
                  f"{tier['commands_per_second_median']:.3f}",
                  '未记录' if write_p95 is None else f"{write_p95 / 1e6:.3f}",
                  f"{tier['physical_peak_delta_bytes_median'] / 1e6:.3f}",
                  f"{tier['physical_retained_delta_bytes_median'] / 1e6:.3f}", tier['errors']]
        markdown.append('| ' + ' | '.join(map(str, values)) + ' |')
    peak = group['effective_peak']
    stable = group['stable_width']
    markdown += ['', f"本组 wall 阈值 {group['wall_ns_limit'] / 1e9:.3f} 秒；"
                  f"按登记规则的有效峰值为 {peak if peak is not None else '未成立'}，"
                  f"前一档为 {stable if stable is not None else '未确定'}。"
                  '此结果仅属于本组方法，不能据此计算相对主表的隔离容量倍数。', '']

args.output.mkdir(parents=True, exist_ok=False)
with (args.output / f'{args.kind}-metrics.csv').open('w', newline='') as stream:
    writer = csv.DictWriter(stream, fieldnames=FIELDS)
    writer.writeheader()
    writer.writerows(rows)
(args.output / f'{args.kind}-tables.md').write_text('\n'.join(markdown).rstrip() + '\n')
(args.output / 'sources.json').write_text(json.dumps(dict(kind=args.kind, inputs=inputs, rows=len(rows),
    script_sha256=hashlib.sha256(Path(__file__).read_bytes()).hexdigest()), indent=2) + '\n')
print(json.dumps(dict(kind=args.kind, rows=len(rows), output=str(args.output))))
