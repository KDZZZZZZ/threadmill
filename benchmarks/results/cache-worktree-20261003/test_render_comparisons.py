"""Exercise report generation through its public CLI, without measurements."""
import csv
import json
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


class ComparisonTables(unittest.TestCase):
    def test_capacity_failure_keeps_missing_metrics_and_later_complete_rows(self):
        # Artificial audit input: the first tier failed before write/collect.
        missing = ['write_p50_ns_median', 'write_p95_ns_median',
                   'collect_p50_ns_median', 'collect_p95_ns_median']
        tiers = []
        for width in [64, 128, 192, 256, 384, 448, 500, 576]:
            tier = dict(agents=width, repeats=3, errors=0, wall_ns_median=1_000_000_000,
                        commands_per_second_median=10,
                        physical_peak_delta_bytes_median=2_000_000,
                        physical_retained_delta_bytes_median=1_000_000)
            for operation in ['write', 'fork', 'collect']:
                for percentile in ['p50', 'p95']:
                    tier[f'{operation}_{percentile}_ns_median'] = 1_000_000
            tiers.append(tier)
        tiers[0]['errors'] = 64
        for name in missing:
            del tiers[0][name]
        group = dict(tiers=tiers, wall_ns_limit=1_250_000_000,
                     effective_peak=None, stable_width=None)
        backend = 'threadmill-historical98-overlay-external'
        audit = dict(rows_verified=48, historical_only=True,
                     summaries={fixture: {'capacity': {backend: group}}
                                for fixture in ['synthetic', 'ipython']})
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            script = root / 'render-comparisons.py'
            shutil.copyfile(Path(__file__).with_name(script.name), script)
            (root / 'legacy-audit').mkdir()
            (root / 'legacy-audit/summary.json').write_text(json.dumps(audit))
            result = subprocess.run([sys.executable, str(script), '--kind', 'legacy',
                                     '--output', str(root / 'output')],
                                    text=True, capture_output=True, timeout=10)
            self.assertEqual(result.returncode, 0, result.stderr)
            with (root / 'output/legacy-metrics.csv').open() as stream:
                rows = list(csv.DictReader(stream))
            self.assertEqual(len(rows), 16)
            self.assertEqual(rows[0]['errors'], '64')
            for name in missing:
                self.assertEqual(rows[0][name], '')
                self.assertEqual(rows[1][name], '1000000')
            self.assertIn('未记录', (root / 'output/legacy-tables.md').read_text())


if __name__ == '__main__':
    unittest.main()
