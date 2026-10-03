import importlib.util
import copy
import pathlib
import subprocess
import sys
import tempfile
import unittest

SPEC = importlib.util.spec_from_file_location("pi_runtime_bench", pathlib.Path(__file__).with_name("bench.py"))
bench = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(bench)


class CapacityRuleTests(unittest.TestCase):
    def test_historical_terminal_rule_requires_an_explicit_callback(self):
        rows = [{"backend": "threadmill-historical98-overlay-external", "agents": 64,
                 "wall_ns": 10, "errors": 0, "closed": closed} for closed in [True, True, True]]
        backend = rows[0]["backend"]
        self.assertIsNone(bench.summarize(rows, 3)[backend]["effective_peak"])
        guard = lambda row: {} if row["closed"] else {"closed": False}
        self.assertEqual(bench.summarize(rows, 3, terminal_errors=guard)[backend]["effective_peak"], 64)
        rows[1]["closed"] = False
        self.assertIsNone(bench.summarize(rows, 3, terminal_errors=guard)[backend]["effective_peak"])

    def test_cleanup_failure_cannot_enter_capacity_with_zero_operation_errors(self):
        healthy = {"backend": "threadmill-external", "agents": 64, "wall_ns": 10, "errors": 0,
                   "execution": {field: 0 for field in ["runtime_cleanup_errors", "runtime_dirs", "active", "queued",
                                                        "heavy_active", "heavy_queued", "tracked_process_groups"]},
                   "vfs": {field: 0 for field in ["materialize_active", "absorb_active", "overlay_active"]}}
        invalid = copy.deepcopy(healthy)
        invalid["agents"] = 128
        invalid["execution"]["runtime_cleanup_errors"] = 1
        rows = [copy.deepcopy(healthy) for _ in range(3)] + [copy.deepcopy(invalid) for _ in range(3)]
        self.assertEqual(bench.summarize(rows, 3)["threadmill-external"]["effective_peak"], 64)
        bench.check_terminal_state(invalid)
        self.assertEqual(invalid["operation_errors"], 0)
        self.assertEqual(invalid["errors"], 1)
        self.assertEqual(invalid["terminal_state_errors"], {"execution.runtime_cleanup_errors": 1})

    def test_missing_threadmill_terminal_counters_fail_closed(self):
        row = {"backend": "threadmill-bwrap", "errors": 0}
        bench.check_terminal_state(row)
        self.assertGreater(row["errors"], 0)
        self.assertIn("execution.runtime_dirs", row["terminal_state_errors"])

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
