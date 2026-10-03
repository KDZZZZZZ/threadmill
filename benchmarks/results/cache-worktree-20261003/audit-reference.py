#!/usr/bin/env python3
"""Audit one 24-row shared-cwd reference against the primary frozen trace."""
import argparse
from collections import Counter
import hashlib
import importlib.util
import json
import math
from pathlib import Path
import sys

sys.dont_write_bytecode = True
cli = argparse.ArgumentParser(description=__doc__)
cli.add_argument('--raw', type=Path, required=True)
cli.add_argument('--primary-registration', type=Path, required=True)
cli.add_argument('--project', type=Path, required=True)
cli.add_argument('--fixture', choices=['synthetic', 'ipython'], required=True)
cli.add_argument('--output', type=Path, required=True)
args = cli.parse_args()
raw, project = args.raw.resolve(), args.project.resolve()
def require(ok, message):
    if not ok:
        raise ValueError(message)
def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()
require(not args.output.exists(), 'output must be new')
primary = json.loads(args.primary_registration.read_text())
reg = json.loads((raw / 'registration.json').read_text())
control = json.loads((raw.parent / 'registration.json').read_text())
require(sha(args.primary_registration) == control['original_registration_sha256'], 'primary registration identity')
require(reg['formal'] is True and not reg['dirty'] and reg['groups'] == ['pi-shared-cwd'], 'formal reference mode')
require(reg['threadmill_commit'] == primary['threadmill_commit'] and reg['tmload_sha256'] == primary['threadmill_binary_sha256'], 'runtime identity')
require(reg['fixture_commit'] == primary['fixtures'][args.fixture]['commit'], 'fixture identity')
require(reg['traces'] == primary['fixtures'][args.fixture]['traces'], 'reference must use exact primary traces')
require(reg['widths'] == primary['widths'] and reg['repeats'] == 3, 'matrix dimensions')
for name, digest in reg['harness_sha256'].items():
    require(sha(project / name) == digest == primary['harness_sha256'][name], 'frozen helper: ' + name)
spec = importlib.util.spec_from_file_location('frozen_runtime', project / 'benchmarks/pi-runtime/bench.py')
runtime = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runtime)
rows, inventory = [], []
for width in reg['widths']:
    path = raw / f'trace-{width}.json'
    require(sha(path) == reg['traces'][str(width)], 'trace SHA')
    trace = json.loads(path.read_text())
    expected = {}
    for agent in trace['agents']:
        aid, ops = agent['id'], agent['operations']
        expected[aid, -1] = {'op': 'fork'}
        expected.update({(aid, i): op for i, op in enumerate(ops)})
        expected[aid, len(ops)] = {'op': 'collect'}
        expected[aid, len(ops) + 1] = {'op': 'release'}
    for repeat in (1, 2, 3):
        path = raw / f'pi-shared-cwd-w{width}-r{repeat}.json'
        row = json.loads(path.read_text())
        require(row['backend'] == 'pi-shared-cwd' and row['agents'] == width and row['repeat'] == repeat, 'row identity: ' + path.name)
        require(row['formal'] is True and row['serial'] is False and row['version'] == 1, 'row mode')
        require(row['fixture_commit'] == reg['fixture_commit'] and row['trace_sha256'] == reg['traces'][str(width)], 'row inputs')
        require(row['errors'] == row['operation_errors'] == row['process_exit'] == 0 and row['terminal_state_errors'] == {}, 'failed reference row')
        require(row['wall_ns'] > 0 and row['cache_oracle_mismatches'] == 0, 'timing/oracle')
        observed = {}
        for op in row['operations']:
            key = op['agent'], op['index']
            require(key in expected and key not in observed and op['op'] == expected[key]['op'], 'operation identity')
            require(type(op['duration_ns']) is int and op['duration_ns'] >= 0 and not op.get('error'), 'operation status/timing')
            if op['op'] == 'bash':
                require(op.get('exit_code', 0) == expected[key].get('expected_exit', 0) and not op.get('cached'), 'bash status')
                require(hashlib.sha256(op['output'].encode()).hexdigest() == op['output_sha256'], 'output SHA')
            observed[key] = op
        require(observed.keys() == expected.keys(), 'complete trace and lifecycle')
        counts = Counter(op['op'] for op in row['operations'])
        require(row['latency'].keys() == counts.keys() and all(row['latency'][k]['count'] == n for k, n in counts.items()), 'latency counts')
        require(math.isclose(row['commands_per_second'], counts['bash'] * 1e9 / row['wall_ns'], rel_tol=1e-9), 'throughput')
        disk = row['physical_disk']
        require(disk['sample_interval_seconds'] == 0.2 and disk['samples'][0] == disk['before'] and disk['samples'][-1] == disk['after'], 'physical sampling')
        require(disk['peak'] == max(disk['samples'], key=lambda sample: sample['df_used_bytes']), 'sampled peak')
        for label, endpoint in [('peak', 'peak'), ('retained', 'after')]:
            require(disk[label + '_delta_bytes'] == disk[endpoint]['df_used_bytes'] - disk['before']['df_used_bytes'], 'physical arithmetic')
        require(disk['before']['btrfs_du_raw'] and disk['after']['btrfs_du_raw'], 'du evidence')
        rows.append(row)
        inventory.append(dict(path=path.name, sha256=sha(path), bytes=path.stat().st_size))
summary = runtime.summarize(rows, 3)
require(summary == json.loads((raw / 'summary.json').read_text())['capacity'], 'saved summary recomputation')
proof = json.loads((raw / 'supervisor.json').read_text())
phase = next(item for item in control['phases'] if item['name'] == raw.name)
require(proof['command'] == phase['argv'] and phase['argv'][1:] == reg['argv'], 'supervised command')
require(proof['complete'] and proof['exit_code'] == 0 and proof['runner']['returncode'] == 0 and not proof['error'] and not proof['stop_signal'], 'supervision status')
require(proof['subreaper'] is True and proof['poll_seconds'] == 0.02 and proof['term_seconds'] == 2.0 and proof['kill_seconds'] == 5.0, 'continuous reaper')
cleanup = proof['cleanup']
require(cleanup['trigger'] == 'runner_exit' and cleanup['clean'] and not any(cleanup[k] for k in ['live_descendants', 'signals', 'remaining', 'errors']), 'supervision cleanup')
result = dict(fixture=args.fixture, reference_only=True, rows_verified=len(rows), errors=0, inventory=inventory, summary=summary,
              audit_script_sha256=sha(Path(__file__)), registration_sha256=sha(raw / 'registration.json'),
              primary_registration_sha256=sha(args.primary_registration), supervisor_sha256=sha(raw / 'supervisor.json'))
args.output.write_text(json.dumps(result, indent=2) + '\n')
print(json.dumps(dict(fixture=args.fixture, rows_verified=len(rows), errors=0, reference_only=True)))
