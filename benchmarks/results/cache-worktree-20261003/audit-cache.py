#!/usr/bin/env python3
"""Audit one complete four-group cache phase against its frozen trace/helpers."""
import argparse
from collections import Counter
import hashlib
import importlib.util
import json
import math
from pathlib import Path
from statistics import median
import sys

sys.dont_write_bytecode = True
cli = argparse.ArgumentParser(description=__doc__)
cli.add_argument('--raw', type=Path, required=True)
cli.add_argument('--project', type=Path, required=True)
cli.add_argument('--output', type=Path, required=True)
args = cli.parse_args()
raw, project = args.raw.resolve(), args.project.resolve()
def require(value, message):
    if not value:
        raise ValueError(message)
def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()
require(not args.output.exists(), 'output must be new')
registration = json.loads((raw / 'registration.json').read_text())
require(registration['formal'] is True and not registration['threadmill_dirty'], 'formal clean source required')
require(registration['agents'] == 16 and registration['repeats'] == 10, 'registered matrix differs')
for name, digest in registration['harness_sha256'].items():
    require(sha(project / name) == digest, 'frozen helper differs: ' + name)
spec = importlib.util.spec_from_file_location('frozen_cache', project / 'benchmarks/cache-runtime/bench.py')
cache = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cache)
trace = json.loads((raw / 'script.json').read_text())
require(sha(raw / 'script.json') == registration['trace_sha256'], 'trace SHA differs')
require(trace['serial'] is True and trace['fixture']['commit'] == registration['fixture_commit'], 'trace fixture/mode differs')
selected = cache.tracing_command(trace)
require(selected == registration['tracing_tax_command'], 'selected command differs')
expected = {}
for agent in trace['agents']:
    aid, ops = agent['id'], agent['operations']
    expected[aid, -1] = {'op': 'fork'}
    expected.update({(aid, i): op for i, op in enumerate(ops)})
    expected[aid, len(ops)] = {'op': 'collect'}
    expected[aid, len(ops) + 1] = {'op': 'release'}
groups = ('cache-off-traced', 'cache-on-traced', 'pi-worktree', 'cache-off-untraced')
rows, inventory, cached = {}, [], []
for group in groups:
    rows[group] = []
    for repeat in range(1, 11):
        path = raw / f'{group}-r{repeat}.json'
        row = json.loads(path.read_text())
        label = path.name
        require(row['group'] == group and row['repeat'] == repeat, 'row identity: ' + label)
        backend = 'pi-worktree' if group == 'pi-worktree' else 'threadmill-' + registration['sandbox']
        require(row['backend'] == backend, 'row backend: ' + label)
        require(row['version'] == 1 and row['agents'] == 16 and row['serial'] is True, 'row mode: ' + label)
        require(row['fixture_commit'] == registration['fixture_commit'] and row['trace_sha256'] == registration['trace_sha256'], 'row inputs: ' + label)
        require(row['process_exit'] == row['errors'] == row['operation_errors'] == 0, 'failed row: ' + label)
        require(row['terminal_state_errors'] == cache.runtime.terminal_state_errors(row) == {}, 'terminal state: ' + label)
        require(row['cache_oracle_mismatches'] == 0 and row['wall_ns'] > 0, 'row correctness/timing: ' + label)
        observed = {}
        for op in row['operations']:
            key = op['agent'], op['index']
            require(key not in observed and key in expected and op['op'] == expected[key]['op'], 'operation identity: ' + label)
            require(type(op['duration_ns']) is int and op['duration_ns'] >= 0 and not op.get('error'), 'operation failure/timing: ' + label)
            if op['op'] == 'bash':
                require(op.get('exit_code', 0) == expected[key].get('expected_exit', 0), 'command exit: ' + label)
                if group != 'pi-worktree':
                    require(op.get('expected_cache') == expected[key]['expected_cache'], 'oracle input: ' + label)
                if group != 'cache-on-traced':
                    require(not op.get('cached'), 'replay in uncached group: ' + label)
                require(hashlib.sha256(op.get('output', '').encode()).hexdigest() == op['output_sha256'], 'output digest: ' + label)
            observed[key] = op
        require(observed.keys() == expected.keys(), 'missing operations: ' + label)
        counts = Counter(op['op'] for op in observed.values())
        require(row['latency'].keys() == counts.keys(), 'latency classes: ' + label)
        require(all(row['latency'][k]['count'] == n for k, n in counts.items()), 'latency counts: ' + label)
        require(math.isclose(row['commands_per_second'], counts['bash'] * 1e9 / row['wall_ns'], rel_tol=1e-9), 'throughput arithmetic: ' + label)
        if group != 'pi-worktree':
            execution = row['execution']
            require(execution['sandbox_backend'] == registration['sandbox'] and execution['capacity'] == 1, 'backend/slots: ' + label)
            require(execution['requests'] == execution['completed'] == counts['bash'], 'completion counters: ' + label)
            require(execution['dependency_tracing'] == (group != 'cache-off-untraced'), 'tracing policy: ' + label)
            snapshot = execution['cache']
            if group == 'cache-on-traced':
                oracle = cache.oracle_report(row)
                require(row['oracle'] == oracle and oracle['unexpected_replay'] == 0, 'unexpected replay: ' + label)
                require(snapshot['lookups'] == counts['bash'] and snapshot['replays'] == oracle['actual_replayed'], 'cache counters: ' + label)
                require(snapshot['verifications'] == snapshot['verification_duration'] == 0, 'microbenchmark verify policy: ' + label)
                total = snapshot['saved_duration'] + snapshot['executed_duration']
                require(math.isclose(snapshot['time_weighted_hit_rate'], snapshot['saved_duration'] / total, rel_tol=1e-9), 'weighted hit arithmetic: ' + label)
                total = snapshot['matched_duration'] + snapshot['executed_duration']
                require(math.isclose(snapshot['time_weighted_reuse_rate'], snapshot['matched_duration'] / total, rel_tol=1e-9), 'weighted reuse arithmetic: ' + label)
                cached.append(dict(repeat=repeat, oracle=oracle, snapshot=snapshot))
            else:
                require(snapshot['lookups'] == snapshot['replays'] == 0, 'cache-off policy: ' + label)
        rows[group].append(row)
        inventory.append(dict(path=path.name, sha256=sha(path), bytes=path.stat().st_size))
for group, name in [('cache-off-traced', 'tracing-on.txt'), ('cache-off-untraced', 'tracing-off.txt')]:
    require((raw / name).read_text() == cache.benchmark_lines(rows[group], registration['language'], selected), 'benchstat sample text: ' + name)
summary = json.loads((raw / 'summary.json').read_text())
medians = {group: median(row['wall_ns'] for row in values) for group, values in rows.items()}
require(summary['wall_ns_median'] == medians, 'wall medians')
command_medians = {group: median(cache.command_ns(row, selected) for row in rows[group]) for group in ('cache-off-traced', 'cache-off-untraced')}
require(summary['command_duration_ns_median'] == command_medians, 'command medians')
require(summary['tracing_tax_ns'] == medians['cache-off-traced'] - medians['cache-off-untraced'], 'tracing tax')
require(summary['net_wall_saved_ns'] == medians['cache-off-untraced'] - medians['cache-on-traced'], 'net wall saved')
require(summary['command_tracing_tax_ns'] == command_medians['cache-off-traced'] - command_medians['cache-off-untraced'], 'command tax')
saved = median(item['snapshot']['saved_duration'] for item in cached)
stores = median(item['snapshot']['store_duration'] for item in cached)
require(summary['historical_service_saved_ns'] == saved and summary['store_duration_ns'] == stores, 'service counters')
require(summary['estimated_net_service_saved_ns'] == saved - summary['tracing_tax_ns'] - stores, 'service decomposition')
control = json.loads((raw.parent / 'registration.json').read_text())
phase = next(item for item in control['phases'] if item['name'] == raw.name)
proof = json.loads((raw / 'supervisor.json').read_text())
require(proof['command'] == phase['argv'] and phase['argv'][1:] == registration['argv'], 'supervised command identity')
require(proof['subreaper'] is True and proof['poll_seconds'] == 0.02 and proof['term_seconds'] == 2.0 and proof['kill_seconds'] == 5.0, 'registered supervisor method')
require(proof['runner']['returncode'] == 0 and proof['cleanup']['trigger'] == 'runner_exit', 'foreground completion')
require(not proof['cleanup']['remaining'] and not proof['cleanup']['errors'], 'cleanup residual/error evidence')
require(proof['complete'] and proof['exit_code'] == 0 and not proof['error'] and not proof['stop_signal'], 'supervisor status')
require(proof['cleanup']['clean'] and not proof['cleanup']['live_descendants'] and not proof['cleanup']['signals'], 'supervisor cleanup')
result = dict(audit_script_sha256=sha(Path(__file__)), control_registration_sha256=sha(raw.parent / 'registration.json'), language=registration['language'], rows_verified=len(inventory), inventory=inventory,
              registration_sha256=sha(raw / 'registration.json'), trace_sha256=sha(raw / 'script.json'),
              supervisor_sha256=sha(raw / 'supervisor.json'), normal_reaped=len(proof['normal_reaped']),
              summary=summary, cached_runs=cached,
              median_time_weighted_hit_rate=median(item['snapshot']['time_weighted_hit_rate'] for item in cached),
              total_expected_reusable=sum(item['oracle']['expected_reusable'] for item in cached),
              total_replayed=sum(item['oracle']['actual_replayed'] for item in cached),
              total_unexpected_replay=sum(item['oracle']['unexpected_replay'] for item in cached),
              shadow_audit=False)
args.output.write_text(json.dumps(result, indent=2) + '\n')
print(json.dumps({key: value for key, value in result.items() if key not in ('inventory', 'cached_runs', 'summary')}))
