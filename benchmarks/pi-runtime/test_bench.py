import importlib.util
import pathlib
import subprocess
import sys
import tempfile
import unittest

SPEC = importlib.util.spec_from_file_location("pi_runtime_bench", pathlib.Path(__file__).with_name("bench.py"))
bench = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(bench)


class CapacityRuleTests(unittest.TestCase):
    def test_formal_cli_rejects_a_smoke_matrix_before_running_commands(self):
        with tempfile.TemporaryDirectory() as temporary:
            completed = subprocess.run([sys.executable, str(pathlib.Path(__file__).with_name("bench.py")), "matrix",
                                        "--root", temporary, "--output", str(pathlib.Path(temporary) / "results"),
                                        "--fixture", "missing-fixture", "--tmload", "missing-binary", "--pi-source", "missing-source",
                                        "--widths", "4", "--repeats", "1"], capture_output=True, text=True)
            self.assertEqual(completed.returncode, 1)
            self.assertIn("formal matrix requires", completed.stderr)
            self.assertFalse((pathlib.Path(temporary) / "results").exists())

    def test_preregistered_rule_uses_medians_and_rejects_any_error(self):
        rows = []
        for width, walls, errors in [
            (64, [1, 1, 9], [0, 0, 0]),
            (128, [1.2, 1.3, 1.2], [0, 0, 0]),
            (192, [1.1, 1.2, 1.1], [0, 1, 0]),
            (256, [1.3, 1.4, 1.5], [0, 0, 0]),
        ]:
            for wall, error in zip(walls, errors):
                rows.append({"backend": "pi-worktree", "agents": width, "wall_ns": wall * 1e9, "errors": error})
        summary = bench.summarize(rows)["pi-worktree"]
        self.assertEqual(summary["effective_peak"], 128)
        self.assertEqual(summary["stable_width"], 64)
        self.assertEqual(summary["tiers"][0]["wall_ns_median"], 1e9)

    def test_error_in_lowest_tier_prevents_a_capacity_claim(self):
        rows = [{"backend": "threadmill-external", "agents": 64, "wall_ns": 10, "errors": 1},
                {"backend": "threadmill-external", "agents": 128, "wall_ns": 9, "errors": 0}]
        self.assertIsNone(bench.summarize(rows)["threadmill-external"]["effective_peak"])

    def test_tier_reports_median_throughput_and_physical_deltas(self):
        rows = [{"backend": "pi-worktree", "agents": 64, "wall_ns": 10, "errors": 0,
                 "commands_per_second": rate,
                 "physical_disk": {"peak_delta_bytes": peak, "retained_delta_bytes": retained}}
                for rate, peak, retained in [(10, 5, 2), (20, 6, 3), (30, 7, 4)]]
        tier = bench.summarize(rows)["pi-worktree"]["tiers"][0]
        self.assertEqual(tier["commands_per_second_median"], 20)
        self.assertEqual(tier["physical_peak_delta_bytes_median"], 6)
        self.assertEqual(tier["physical_retained_delta_bytes_median"], 3)


if __name__ == "__main__":
    unittest.main()
