import importlib.util
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

SPEC = importlib.util.spec_from_file_location("cache_runtime_bench", Path(__file__).with_name("bench.py"))
bench = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(bench)


class CacheScriptTests(unittest.TestCase):
    def test_serial_script_has_a_cold_producer_and_known_repeats(self):
        trace = bench.script_trace("pytest", "a" * 40, 2, 42, 0, "original source", "changed source", "python3")
        self.assertTrue(trace["serial"])
        first = [op for op in trace["agents"][0]["operations"] if op["op"] == "bash"]
        second = [op for op in trace["agents"][1]["operations"] if op["op"] == "bash"]
        self.assertEqual([op["expected_cache"] for op in first], [False, True, False, True])
        self.assertTrue(all(op["expected_cache"] for op in second))
        self.assertEqual(first[0]["command"], second[0]["command"])

    def test_tracing_tax_selects_the_same_first_full_test_command_in_every_repeat(self):
        trace = bench.script_trace("pytest", "a" * 40, 2, 42, 0, "original", "changed", "python3")
        selected = bench.tracing_command(trace)
        self.assertEqual(selected["agent"], "agent-0")
        self.assertEqual(selected["index"], 1)
        self.assertEqual(selected["command"], trace["agents"][0]["operations"][1]["command"])
        results = [{"wall_ns": 1000 + duration, "operations": [
            {"agent": "agent-1", "index": 1, "op": "bash", "duration_ns": 999},
            {"agent": "agent-0", "index": 2, "op": "bash", "duration_ns": 888},
            {"agent": "agent-0", "index": 1, "op": "bash", "duration_ns": duration},
        ]} for duration in [101, 102]]
        lines = bench.benchmark_lines(results, "pytest", selected).splitlines()
        self.assertEqual(lines, ["BenchmarkReplay/pytest-1 1 1101 ns/op", "BenchmarkCommand/pytest-1 1 101 ns/op",
                                 "BenchmarkReplay/pytest-1 1 1102 ns/op", "BenchmarkCommand/pytest-1 1 102 ns/op"])

    def test_manifest_pins_binary_trace_protocol_and_pi_build(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            for name in ["tmload", "protocol", "trace", ".threadmill-benchmark-build.json"]:
                (root / name).write_bytes(b"abc")
            pinned = bench.frozen_inputs(root / "tmload", root / "trace", root / "protocol", root)
            expected = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
            for field in ["tmload_sha256", "trace_sha256", "registration_sha256", "pi_build_manifest_sha256"]:
                self.assertEqual(pinned[field], expected)

    def test_formal_cli_requires_benchstat_before_running_commands(self):
        with tempfile.TemporaryDirectory() as temporary:
            completed = subprocess.run([sys.executable, str(Path(__file__).with_name("bench.py")),
                                        "--language", "go", "--root", temporary, "--output", str(Path(temporary) / "results"),
                                        "--tmload", "missing-binary", "--pi-source", "missing-source"],
                                       env={**os.environ, "PATH": "/usr/bin:/bin"}, capture_output=True, text=True)
            self.assertEqual(completed.returncode, 1)
            self.assertIn("benchstat", completed.stderr)
            self.assertFalse((Path(temporary) / "go-fixture").exists())


if __name__ == "__main__":
    unittest.main()
