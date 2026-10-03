#!/usr/bin/env python3
"""Audit saved primary rows without changing measurements or registrations."""
import argparse
import collections
import datetime
import hashlib
import importlib.util
import json
import math
from pathlib import Path
import sys

sys.dont_write_bytecode = True
parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--raw', type=Path, required=True)
parser.add_argument('--project', type=Path, required=True)
parser.add_argument('--output', type=Path, required=True)
args = parser.parse_args()
BASE, PROJECT = args.raw.resolve(), args.project.resolve()
registration = json.loads((BASE / 'preregistration.json').read_text())
helper = PROJECT / 'benchmarks/pi-runtime/bench.py'
if hashlib.sha256(helper.read_bytes()).hexdigest() != registration['harness_sha256']['benchmarks/pi-runtime/bench.py']:
    raise SystemExit('frozen runtime helper SHA mismatch')
if args.output.exists():
    raise SystemExit('audit output must be a new file')
spec = importlib.util.spec_from_file_location('frozen_runtime', helper)
runtime = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runtime)
failures, missing, inventory, summaries, descriptive = [], [], [], {}, {}

def check(ok, label, message):
    if not ok:
        failures.append({'row': label, 'check': message})

def nonnegative_int(value):
    return type(value) is int and value >= 0

for fixture in ['synthetic', 'ipython']:
    rows, uninterrupted = [], []
    for width in registration['widths']:
        trace_path = BASE / fixture / f'trace-{width}.json'
        trace_bytes = trace_path.read_bytes()
        trace_sha = hashlib.sha256(trace_bytes).hexdigest()
        check(trace_sha == registration['fixtures'][fixture]['traces'][str(width)], str(trace_path), 'frozen trace SHA')
        trace = json.loads(trace_bytes)
        expected = {}
        for agent in trace['agents']:
            aid, operations = agent['id'], agent['operations']
            expected[aid, -1] = {'op': 'fork'}
            expected.update({(aid, i): op for i, op in enumerate(operations)})
            expected[aid, len(operations)] = {'op': 'collect'}
            expected[aid, len(operations) + 1] = {'op': 'release'}
        for group in ['threadmill-bwrap', 'threadmill-external', 'pi-worktree']:
            for repeat in [1, 2, 3]:
                label = f'{fixture}/{group}-w{width}-r{repeat}'
                path = BASE / (label + '.json')
                recovered = BASE / 'continuation-20261003' / fixture / path.name
                if not path.exists() and recovered.exists():
                    path = recovered
                if not path.exists():
                    missing.append(label)
                    continue
                raw = path.read_bytes()
                row = json.loads(raw)
                inventory.append({'path': str(path.relative_to(BASE)), 'sha256': hashlib.sha256(raw).hexdigest(), 'bytes': len(raw)})
                check(row.get('backend') == group and row.get('agents') == width and row.get('repeat') == repeat, label, 'matrix identity')
                check(row.get('formal') is True and row.get('version') == 1, label, 'formal version')
                check(row.get('fixture_commit') == registration['fixtures'][fixture]['commit'], label, 'fixture SHA')
                check(row.get('trace_sha256') == trace_sha, label, 'result trace SHA')
                check(row.get('serial') is False, label, 'parallel trace mode')
                check(row.get('process_exit') == 0 and row.get('errors') == 0 and row.get('operation_errors') == 0, label, 'successful operations and process')
                check(row.get('terminal_state_errors') == {} and runtime.terminal_state_errors(row) == {}, label, 'complete clean terminal proof')
                check(row.get('cache_oracle_mismatches') == 0, label, 'cache oracle')
                check(nonnegative_int(row.get('wall_ns')) and row['wall_ns'] > 0 and nonnegative_int(row.get('setup_ns')), label, 'wall/setup durations')
                observed = {}
                by_type = collections.Counter()
                for operation in row['operations']:
                    key = operation['agent'], operation['index']
                    check(key not in observed, label, 'unique operation identity')
                    observed[key] = operation
                    wanted = expected.get(key)
                    check(wanted is not None and operation['op'] == wanted['op'], label, 'operation kind matches exact trace index')
                    check(nonnegative_int(operation.get('duration_ns')), label, 'finite nonnegative operation duration')
                    check(not operation.get('error'), label, 'no operation error')
                    if operation['op'] == 'bash' and wanted is not None:
                        check(operation.get('exit_code', 0) == wanted.get('expected_exit', 0), label, 'expected bash exit')
                    by_type[operation['op']] += 1
                check(observed.keys() == expected.keys(), label, 'all trace and lifecycle operations exactly once')
                check(row['latency'].keys() == by_type.keys(), label, 'latency operation classes')
                for kind, value in row['latency'].items():
                    check(value['count'] == by_type[kind] and nonnegative_int(value['p50_ns']) and nonnegative_int(value['p95_ns']), label, 'latency counts/durations')
                rate = row.get('commands_per_second')
                check(type(rate) in (int, float) and math.isfinite(rate) and math.isclose(rate, by_type['bash'] * 1e9 / row['wall_ns'], rel_tol=1e-9), label, 'throughput matches completed bash count and wall')
                disk = row['physical_disk']
                samples = disk['samples']
                check(disk['sample_interval_seconds'] == 0.2 and len(samples) >= 2, label, 'registered physical sampling interval')
                check(samples[0] == disk['before'] and samples[-1] == disk['after'], label, 'physical sample endpoints')
                check(disk['peak'] == max(samples, key=lambda x: x['df_used_bytes']), label, 'physical observed peak')
                check(disk['peak_delta_bytes'] == disk['peak']['df_used_bytes'] - disk['before']['df_used_bytes'], label, 'peak delta arithmetic')
                check(disk['retained_delta_bytes'] == disk['after']['df_used_bytes'] - disk['before']['df_used_bytes'], label, 'retained delta arithmetic')
                check(all(type(s['df_used_bytes']) is int and nonnegative_int(s['time_ns']) for s in samples), label, 'physical sample numeric fields')
                check(bool(disk['before'].get('btrfs_du_raw')) and bool(disk['after'].get('btrfs_du_raw')), label, 'physical du evidence')
                if group.startswith('threadmill-'):
                    check(row['execution']['capacity'] == 8 and row['execution']['sandbox_backend'] == group.removeprefix('threadmill-'), label, 'registered execution backend/slots')
                    check(row['execution']['dependency_tracing'] is False and row['execution']['cache']['lookups'] == 0, label, 'runtime-only cache/trace settings')
                    check(row['execution']['requests'] == by_type['bash'] and row['execution']['completed'] == by_type['bash'], label, 'execution request completion counts')
                rows.append(row)
                if path != recovered:
                    uninterrupted.append(row)
                else:
                    check(row.get('attempt') == 2 and row.get('prior_interrupted_attempt') == 'interruption-20261003/observation.json', label, 'supplemental attempt retains interrupted predecessor')
    summaries[fixture] = runtime.summarize(uninterrupted, 3)
    descriptive[fixture] = {group: {'tiers': value['tiers']}
                            for group, value in runtime.summarize(rows, 3).items()}
    if fixture == 'ipython':
        for summary in [summaries[fixture], descriptive[fixture]]:
            for tier in summary['threadmill-external']['tiers']:
                if tier['agents'] == 576:
                    tier.update(interrupted_attempts=1, uninterrupted_completed_repeats=2,
                                supplemental_completed_repeats=len(rows) - len(uninterrupted),
                                formal_capacity_eligible=False,
                                caveat='Two original completed repeats, one unresolved interrupted attempt; any continuation is supplemental, never a replacement success.')

result = {'audited_utc': datetime.datetime.now(datetime.timezone.utc).isoformat(), 'completed_rows': len(inventory), 'expected_rows': 144, 'missing': missing, 'failures': failures, 'inventory': inventory, 'summaries': summaries, 'descriptive_summaries_including_supplemental': descriptive, 'capacity_basis': 'Original uninterrupted repeats only; supplemental IPython external576 repeat is not used to replace the unknown interrupted attempt in capacity acceptance.', 'interrupted_attempt_count': 1, 'interruption_record': 'interruption-20261003/observation.json'}
output = args.output
output.write_text(json.dumps(result, indent=2) + '\n')
print(json.dumps({'completed': len(inventory), 'missing': missing, 'failed_checks': len(failures), 'failure_examples': failures[:10], 'artifact': str(output)}))
sys.exit(bool(failures or missing))
