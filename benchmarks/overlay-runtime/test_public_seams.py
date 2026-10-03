"""Public CLI checks; native tests require an explicit real-filesystem manifest."""

import hashlib
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest

HERE = Path(__file__).resolve().parent
BASE = "98ed7d3d38c0fc956de0bc70c858f4ef65c90e02"


def process_children(pid):
    # Linux exposes children per thread; Go may spawn bash from any OS thread.
    found = set()
    for task in Path(f"/proc/{pid}/task").glob("*/children"):
        try:
            found.update(int(value) for value in task.read_text().split())
        except (FileNotFoundError, ProcessLookupError):
            pass
    return sorted(found)


class FinalRowCLI(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.trace = {"version": 1, "fixture": {"commit": "a" * 40}, "agents": [
            {"id": "agent-0", "operations": [{"op": "bash", "command": "true", "expected_exit": 0}]}]}
        self.write("trace", self.trace)
        trace_sha = hashlib.sha256((self.root / "trace.json").read_bytes()).hexdigest()
        self.replay = {"version": 1, "backend": "threadmill-historical98-overlay-external", "runtime_commit": BASE,
                       "fixture_commit": "a" * 40, "trace_sha256": trace_sha, "agents": 1, "wall_ns": 100,
                       "errors": 0, "serial": False, "setup_ns": 20, "commands_per_second": 10_000_000.0,
                       "latency": {"fork": {"count": 1, "p50_ns": 10, "p95_ns": 10},
                                   "bash": {"count": 1, "p50_ns": 20, "p95_ns": 20},
                                   "collect": {"count": 1, "p50_ns": 30, "p95_ns": 30},
                                   "release": {"count": 1, "p50_ns": 30, "p95_ns": 30}},
                       "execution": dict.fromkeys([
                           "queued", "active", "heavy_queued", "heavy_active", "tracked_process_groups", "runtime_dirs",
                           "adapter_reap_errors", "adapter_orphan_runtime_dirs", "adapter_runtime_dir_scan_errors"], 0),
                       "vfs": {"materialize_active": 0, "absorb_active": 0, "overlay_active": 0,
                               "overlay_available": True, "overlay_backend": "native-overlayfs",
                               "overlay_error_fallbacks": 0, "materialize_full_copies": 0, "materialize_reflinks": 0},
                       "checkpoint_ids": ["checkpoint-agent-0"], "checkpoint_overlay_proofs": 1,
                       "checkpoint_close_proof_errors": 0,
                       "final_close_ns": 10, "final_close_errors": 0,
                       "checkpoint_audit": "pending; must pass audit-only before acceptance",
                       "operations": [{"agent": "agent-0", "index": -1, "op": "fork", "exit_code": 0, "duration_ns": 10},
                                      {"agent": "agent-0", "index": 0, "op": "bash", "exit_code": 0, "duration_ns": 20},
                                      {"agent": "agent-0", "index": 1, "op": "collect", "exit_code": 0, "duration_ns": 30},
                                      {"agent": "agent-0", "index": 2, "op": "release", "exit_code": 0, "duration_ns": 30}]}
        self.write("replay", self.replay)
        self.measured = json.loads(json.dumps(self.replay))
        self.measured.update(process_exit=0, physical_disk={
            "before": {"df_used_bytes": 10, "btrfs_du_raw": "before shared/exclusive sample"},
            "peak": {"df_used_bytes": 15}, "after": {"df_used_bytes": 13, "btrfs_du_raw": "after shared/exclusive sample"},
            "samples": [{"df_used_bytes": 10, "btrfs_du_raw": "before shared/exclusive sample"}, {"df_used_bytes": 15},
                        {"df_used_bytes": 13, "btrfs_du_raw": "after shared/exclusive sample"}],
            "sample_interval_seconds": 0.2, "peak_delta_bytes": 5, "retained_delta_bytes": 3})
        self.write("measured", self.measured)
        self.audit = {"version": 1, "runtime_commit": BASE, "fixture_commit": "a" * 40,
                      "trace_sha256": trace_sha, "replay_report_sha256": hashlib.sha256((self.root / "replay.json").read_bytes()).hexdigest(),
                      "passed": True, "errors": 0, "checkpoint_count": 1, "checkpoint_overlay_proofs": 1,
                      "audit_ns": 50, "lower_tree_sha256": "b" * 64,
                      "vfs": {"materialize_active": 0, "absorb_active": 0, "overlay_active": 0}}
        self.write("audit", self.audit)
        self.launch = {"version": 1, "process_exit": 0, "signal": 0, "errors": 0, "parent_overlay_mounts_after": [],
                       "known_children_gone": True, "namespace": {"euid": 0, "pid": 1, "mount_namespace": "mnt:[222]",
                       "parent_mount_namespace": "mnt:[111]", "overlay_mounts_after": [], "children_after": [],
                       "binary_exit": 0, "readonly_sources": True}}
        self.write("launch", self.launch)

    def write(self, name, value):
        (self.root / (name + ".json")).write_text(json.dumps(value) + "\n")

    def verify(self, label="final"):
        target = self.root / (label + ".json")
        result = subprocess.run([sys.executable, HERE / "bench.py", "verify-row", "--trace", self.root / "trace.json",
                                 "--replay", self.root / "replay.json", "--measured", self.root / "measured.json",
                                 "--audit", self.root / "audit.json", "--replay-launch", self.root / "launch.json",
                                 "--audit-launch", self.root / "launch.json", "--json-out", target], capture_output=True, text=True)
        return result.returncode, json.loads(target.read_text())

    def test_complete_evidence_accepts_without_fabricating_new_runtime_counter(self):
        code, row = self.verify()
        self.assertEqual(code, 0)
        self.assertEqual(row["errors"], 0)
        self.assertEqual(row["checkpoint_audit"], "passed")
        self.assertNotIn("runtime_cleanup_errors", row["execution"])

    def test_missing_audit_is_a_method_failure(self):
        (self.root / "audit.json").unlink()
        code, row = self.verify()
        self.assertNotEqual(code, 0)
        self.assertGreater(row["errors"], 0)
        self.assertTrue(row["method_errors"])

    def test_reap_and_unmounted_proof_cannot_pass_as_zero(self):
        del self.replay["execution"]["adapter_orphan_runtime_dirs"]
        self.replay["checkpoint_close_proof_errors"] = 1
        self.write("replay", self.replay)
        code, row = self.verify()
        self.assertNotEqual(code, 0)
        self.assertGreater(row["errors"], 0)

    def test_namespace_teardown_cannot_hide_runtime_close_failure(self):
        self.launch["namespace"]["overlay_mounts_after"] = ["/dedicated/checkpoint"]
        self.write("launch", self.launch)
        code, row = self.verify()
        self.assertNotEqual(code, 0)
        self.assertGreater(row["errors"], 0)

    def test_missing_physical_du_is_a_method_failure(self):
        del self.measured["physical_disk"]["after"]["btrfs_du_raw"]
        self.write("measured", self.measured)
        code, row = self.verify()
        self.assertNotEqual(code, 0)
        self.assertEqual(row["failure_kind"], "method")

    def test_close_error_with_zero_active_stats_is_a_method_failure(self):
        self.replay.update(final_close_errors=1, errors=1)
        self.write("replay", self.replay)
        self.measured.update(self.replay, process_exit=1)
        self.write("measured", self.measured)
        self.launch["process_exit"] = 1
        self.launch["namespace"]["binary_exit"] = 1
        self.write("launch", self.launch)
        code, row = self.verify()
        self.assertNotEqual(code, 0)
        self.assertEqual(row["failure_kind"], "method")
        self.assertIn("final_close_errors", row["method_errors"])

    def test_missing_actual_directory_scan_proof_is_a_method_failure(self):
        del self.replay["execution"]["adapter_runtime_dir_scan_errors"]
        self.write("replay", self.replay)
        code, row = self.verify()
        self.assertNotEqual(code, 0)
        self.assertEqual(row["failure_kind"], "method")

    def test_nonzero_command_is_retained_as_capacity_failure(self):
        self.replay["errors"] = 1
        self.replay["operations"][1].update(exit_code=124, error="command timed out")
        self.write("replay", self.replay)
        self.measured.update(self.replay, process_exit=1)
        self.write("measured", self.measured)
        self.launch["process_exit"] = 1
        self.launch["namespace"]["binary_exit"] = 1
        self.write("launch", self.launch)
        code, row = self.verify()
        self.assertNotEqual(code, 0)
        self.assertEqual(row["failure_kind"], "capacity")
        self.assertFalse(row["method_errors"])

    def test_measurement_cannot_change_raw_error_or_performance_fields(self):
        baseline = json.loads(json.dumps(self.measured))
        for field, changed in [("errors", 1), ("error", "invented failure"), ("serial", True),
                               ("latency", {}), ("commands_per_second", 999.0), ("setup_ns", 21)]:
            with self.subTest(field=field):
                measured = {**baseline, field: changed}
                self.write("measured", measured)
                code, row = self.verify("changed-" + field)
                self.assertNotEqual(code, 0)
                self.assertEqual(row["failure_kind"], "method")
                self.assertIn("measured/raw mismatch " + field, row["method_errors"])

    def test_extra_or_missing_trace_steps_are_method_failures(self):
        expected = json.loads(json.dumps(self.replay["operations"]))
        cases = {"extra": expected + [{"agent": "agent-0", "index": 9, "op": "bash", "duration_ns": 20, "exit_code": 0}],
                 "unknown-agent": expected + [{"agent": "untraced", "index": -1, "op": "fork", "duration_ns": 10}],
                 "missing": [expected[0], expected[2], expected[3]]}
        for label, operations in cases.items():
            with self.subTest(case=label):
                self.replay["operations"] = operations
                self.write("replay", self.replay)
                self.measured.update(self.replay)
                self.write("measured", self.measured)
                code, row = self.verify(label)
                self.assertNotEqual(code, 0)
                self.assertEqual(row["failure_kind"], "method")
                self.assertIn("operation key set differs from trace", row["method_errors"])

    def test_serial_boolean_cannot_be_replaced_with_numeric_zero(self):
        self.measured["serial"] = 0
        self.write("measured", self.measured)
        code, row = self.verify()
        self.assertNotEqual(code, 0)
        self.assertEqual(row["failure_kind"], "method")
        self.assertIn("measured/raw mismatch serial", row["method_errors"])

    def test_missing_raw_performance_fields_cannot_default_to_zero(self):
        del self.replay["commands_per_second"]
        del self.measured["commands_per_second"]
        self.write("replay", self.replay)
        self.write("measured", self.measured)
        code, row = self.verify()
        self.assertNotEqual(code, 0)
        self.assertEqual(row["failure_kind"], "method")
        self.assertIn("missing raw field commands_per_second", row["method_errors"])

    def test_matching_raw_and_measured_scalar_metrics_must_be_finite_and_nonnegative(self):
        baseline = json.loads(json.dumps(self.replay))
        measured = json.loads(json.dumps(self.measured))
        cases = [("setup-missing", "setup_ns", None, "invalid setup timing"),
                 ("setup-negative", "setup_ns", -1, "invalid setup timing"),
                 ("setup-infinite", "setup_ns", float("inf"), "invalid setup timing"),
                 ("setup-boolean", "setup_ns", True, "invalid setup timing"),
                 ("throughput-missing", "commands_per_second", None, "invalid command throughput"),
                 ("throughput-negative", "commands_per_second", -1.0, "invalid command throughput"),
                 ("throughput-infinite", "commands_per_second", float("inf"), "invalid command throughput"),
                 ("throughput-boolean", "commands_per_second", True, "invalid command throughput")]
        for label, field, value, expected_error in cases:
            with self.subTest(case=label):
                self.replay = json.loads(json.dumps(baseline))
                self.measured = json.loads(json.dumps(measured))
                if value is None:
                    del self.replay[field]
                    del self.measured[field]
                else:
                    self.replay[field] = self.measured[field] = value
                self.write("replay", self.replay)
                self.write("measured", self.measured)
                self.audit["replay_report_sha256"] = hashlib.sha256((self.root / "replay.json").read_bytes()).hexdigest()
                self.write("audit", self.audit)
                code, row = self.verify(label)
                self.assertNotEqual(code, 0)
                self.assertEqual(row["failure_kind"], "method")
                self.assertIn(expected_error, row["method_errors"])

    def test_matching_latency_requires_observed_classes_and_valid_integer_samples(self):
        baseline = json.loads(json.dumps(self.replay))
        measured = json.loads(json.dumps(self.measured))
        latency = baseline["latency"]
        cases = [("latency-missing", None, "invalid latency evidence"),
                 ("latency-empty", {}, "latency operation classes differ"),
                 ("latency-unexpected", {**latency, "think": {"count": 1, "p50_ns": 10, "p95_ns": 10}},
                  "latency operation classes differ"),
                 ("latency-fields-missing", {**latency, "bash": {"count": 1}}, "invalid latency bash"),
                 ("latency-negative", {**latency, "bash": {"count": 1, "p50_ns": -1, "p95_ns": 20}},
                  "invalid latency bash"),
                 ("latency-infinite", {**latency, "bash": {"count": 1, "p50_ns": 20, "p95_ns": float("inf")}},
                  "invalid latency bash"),
                 ("latency-wrong-count", {**latency, "bash": {"count": 2, "p50_ns": 20, "p95_ns": 20}},
                  "invalid latency bash"),
                 ("latency-boolean-count", {**latency, "bash": {"count": True, "p50_ns": 20, "p95_ns": 20}},
                  "invalid latency bash")]
        for label, value, expected_error in cases:
            with self.subTest(case=label):
                self.replay = json.loads(json.dumps(baseline))
                self.measured = json.loads(json.dumps(measured))
                if value is None:
                    del self.replay["latency"]
                    del self.measured["latency"]
                else:
                    self.replay["latency"] = self.measured["latency"] = value
                self.write("replay", self.replay)
                self.write("measured", self.measured)
                self.audit["replay_report_sha256"] = hashlib.sha256((self.root / "replay.json").read_bytes()).hexdigest()
                self.write("audit", self.audit)
                code, row = self.verify(label)
                self.assertNotEqual(code, 0)
                self.assertEqual(row["failure_kind"], "method")
                self.assertIn(expected_error, row["method_errors"])

    def test_missing_negative_or_nonfinite_step_durations_fail(self):
        for label, duration in [("missing", None), ("negative", -1), ("nan", float("nan")), ("infinite", float("inf"))]:
            with self.subTest(case=label):
                if duration is None:
                    del self.replay["operations"][1]["duration_ns"]
                else:
                    self.replay["operations"][1]["duration_ns"] = duration
                self.write("replay", self.replay)
                self.measured.update(self.replay)
                self.write("measured", self.measured)
                code, row = self.verify("duration-" + label)
                self.assertNotEqual(code, 0)
                self.assertEqual(row["failure_kind"], "method")
                self.assertIn("invalid operation duration agent-0:0", row["method_errors"])

    def test_physical_deltas_use_fixed_before_peak_after_arithmetic(self):
        expected = json.loads(json.dumps(self.measured["physical_disk"]))
        for label, changed in [("peak-delta", {"peak_delta_bytes": 4}), ("retained-delta", {"retained_delta_bytes": 2}),
                               ("peak-below-after", {"peak": {"df_used_bytes": 12}, "peak_delta_bytes": 2})]:
            with self.subTest(case=label):
                self.measured["physical_disk"] = {**expected, **changed}
                self.write("measured", self.measured)
                code, row = self.verify(label)
                self.assertNotEqual(code, 0)
                self.assertEqual(row["failure_kind"], "method")
                self.assertIn("physical allocation arithmetic differs", row["method_errors"])

    def test_peak_must_exist_in_the_actual_saved_samples(self):
        self.measured["physical_disk"].update(peak={"df_used_bytes": 16}, peak_delta_bytes=6)
        self.write("measured", self.measured)
        code, row = self.verify()
        self.assertNotEqual(code, 0)
        self.assertEqual(row["failure_kind"], "method")
        self.assertIn("physical sampled peak differs from saved samples", row["method_errors"])

    def test_fork_failure_allows_only_the_declared_body_and_collect_skip(self):
        self.replay.update(errors=2, checkpoint_ids=[], checkpoint_overlay_proofs=0, commands_per_second=0.0,
                           latency={"fork": {"count": 1, "p50_ns": 10, "p95_ns": 10},
                                    "release": {"count": 1, "p50_ns": 30, "p95_ns": 30}},
                           operations=[{"agent": "agent-0", "index": -1, "op": "fork", "duration_ns": 10,
                                        "error": "fork failed", "exit_code": 0},
                                       {"agent": "agent-0", "index": 2, "op": "release", "duration_ns": 30, "exit_code": 0}])
        self.write("replay", self.replay)
        self.measured.update(self.replay, process_exit=1)
        self.write("measured", self.measured)
        self.launch["process_exit"] = 1
        self.launch["namespace"]["binary_exit"] = 1
        self.write("launch", self.launch)
        code, row = self.verify()
        self.assertNotEqual(code, 0)
        self.assertEqual(row["failure_kind"], "capacity")
        self.assertFalse(row["method_errors"])

    def test_consistent_replay_error_cannot_be_reported_as_zero_errors(self):
        self.replay["error"] = self.measured["error"] = "unreported replay failure"
        self.write("replay", self.replay)
        self.write("measured", self.measured)
        self.audit["replay_report_sha256"] = hashlib.sha256((self.root / "replay.json").read_bytes()).hexdigest()
        self.write("audit", self.audit)
        code, row = self.verify()
        self.assertNotEqual(code, 0)
        self.assertEqual(row["failure_kind"], "method")
        self.assertIn("unreported replay error", row["method_errors"])

    def test_different_identity_or_non_native_copy_cannot_be_accepted(self):
        for field, wrong in [("fixture_commit", "c" * 40), ("trace_sha256", "c" * 64),
                             ("replay_report_sha256", "c" * 64)]:
            with self.subTest(field=field):
                changed = {**self.audit, field: wrong}
                self.write("audit", changed)
                code, row = self.verify(field)
                self.assertNotEqual(code, 0)
                self.assertIn("audit " + field, row["method_errors"])
        self.write("audit", self.audit)
        self.replay["vfs"]["materialize_full_copies"] = 1
        self.measured.update(self.replay)
        self.write("replay", self.replay)
        self.write("measured", self.measured)
        code, row = self.verify("copy")
        self.assertNotEqual(code, 0)
        self.assertIn("non-native materialization: materialize_full_copies", row["method_errors"])

    def test_matrix_cli_requires_the_dedicated_lower_loop_condition(self):
        result = subprocess.run([sys.executable, HERE / "bench.py", "matrix", "--project", self.root,
                                 "--main-registration", self.root / "registration.json", "--trace-dir", self.root,
                                 "--fixture", self.root, "--lower", self.root, "--root", self.root,
                                 "--output", self.root, "--binary", self.root / "binary",
                                 "--provenance", self.root / "provenance.json", "--dedicated-volume"],
                                capture_output=True, text=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("--dedicated-lower-loop", result.stderr)


@unittest.skipUnless(os.environ.get("TMLEGACY_SEAM_MANIFEST"), "requires explicit native integration manifest")
class NativeCLI(unittest.TestCase):
    def test_unsupported_shell_and_cache_traces_stop_before_operations(self):
        spec = json.loads(Path(os.environ["TMLEGACY_SEAM_MANIFEST"]).read_text())
        for label, operation, message in [
            ("shell", {"op": "bash", "command": "echo unsupported"}, "unsupported stage-3 command"),
            ("cache", {"op": "bash", "command": "true", "expected_cache": True}, "cache scripts")]:
            with self.subTest(kind=label):
                root = Path(spec["scratch"] + "-" + label)
                root.mkdir()
                self.addCleanup(__import__("shutil").rmtree, root)
                trace = {"version": 1, "fixture": {"kind": "repository", "commit": spec["fixture_commit"]},
                         "agents": [{"id": "agent-0", "operations": [operation]}]}
                source = root / "trace.json"
                source.write_text(json.dumps(trace) + "\n")
                report = root / "replay.json"
                result = subprocess.run(["/usr/bin/setpriv", "--pdeathsig", "KILL", sys.executable, HERE / "launch.py",
                    "--binary", spec["binary"], "--binary-sha256", spec["binary_sha256"], "--trace", source,
                    "--trace-sha256", hashlib.sha256(source.read_bytes()).hexdigest(), "--repo", spec["fixture"],
                    "--lower", spec["lower"], "--live-root", root / "live", "--json-out", report,
                    "--evidence-out", root / "launch.json"], capture_output=True, text=True)
                self.assertNotEqual(result.returncode, 0)
                row = json.loads(report.read_text())
                self.assertIn(message, row["error"])
                self.assertGreater(row["errors"], 0)
                self.assertFalse(row["operations"])
                self.assertEqual(list((root / "live").iterdir()), [])

    def test_sigterm_kills_namespace_tree_and_records_failure(self):
        self.interrupt_launcher(signal.SIGTERM)

    def test_parent_death_kills_namespace_tree(self):
        self.interrupt_launcher(signal.SIGKILL)

    def interrupt_launcher(self, signum):
        spec = json.loads(Path(os.environ["TMLEGACY_SEAM_MANIFEST"]).read_text())
        root = Path(spec["scratch"] + "-signal-" + str(signum))
        root.mkdir(exist_ok=False)
        self.addCleanup(__import__("shutil").rmtree, root)
        trace = {"version": 1, "seed": 0, "fixture": {"kind": "repository", "files": 1, "file_bytes": 0, "commit": spec["fixture_commit"]},
                 "agents": [{"id": "agent-0", "operations": [{"op": "bash", "command": "sleep 5.000000", "expected_exit": 0}]}]}
        source = root / "trace.json"
        source.write_text(json.dumps(trace) + "\n")
        evidence = root / "launch.json"
        process = subprocess.Popen(["/usr/bin/setpriv", "--pdeathsig", "KILL", sys.executable, HERE / "launch.py",
                    "--binary", spec["binary"], "--binary-sha256", spec["binary_sha256"], "--trace", source,
                    "--trace-sha256", hashlib.sha256(source.read_bytes()).hexdigest(), "--repo", spec["fixture"],
                    "--lower", spec["lower"], "--live-root", root / "live", "--json-out", root / "replay.json",
                    "--evidence-out", evidence])
        try:
            deadline = time.monotonic() + 5
            while not Path(str(evidence) + ".parent-ready").exists() and time.monotonic() < deadline:
                time.sleep(0.01)
            self.assertTrue(Path(str(evidence) + ".parent-ready").exists())
            # Observe the process tree through the public OS seam before the
            # supervisor disappears; SIGKILL cannot write final evidence.
            unshare = process_children(process.pid)
            self.assertEqual(len(unshare), 1)
            init = process_children(unshare[0])
            self.assertEqual(len(init), 1)
            deadline = time.monotonic() + 5
            worker, command = [], []
            while time.monotonic() < deadline:
                command = []
                worker = process_children(init[0])
                if len(worker) == 1:
                    command = process_children(worker[0])
                mounts = Path(f"/proc/{init[0]}/mountinfo").read_text().splitlines()
                if command and any(str(root / "live") in line and " - overlay " in line for line in mounts):
                    break
                time.sleep(0.01)
            self.assertTrue(command, "requires a real command child before killing the launcher")
            self.assertTrue(any(str(root / "live") in line and " - overlay " in line for line in mounts))
            starts = {pid: Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()[19]
                      for pid in unshare + init + worker + command}
            process.send_signal(signum)
            self.assertNotEqual(process.wait(timeout=10), 0)
            deadline = time.monotonic() + 5
            while time.monotonic() < deadline:
                remaining = []
                for pid, start in starts.items():
                    try:
                        current = Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()[19]
                    except (FileNotFoundError, ProcessLookupError):
                        continue
                    if current == start:
                        remaining.append(pid)
                if not remaining:
                    break
                time.sleep(0.02)
            self.assertEqual(remaining, [])
            if signum == signal.SIGTERM:
                proof = json.loads(evidence.read_text())
                self.assertEqual(proof["signal"], signum)
                self.assertTrue(proof["known_children_gone"])
                self.assertEqual(proof["parent_overlay_mounts_after"], [])
            else:
                self.assertFalse(evidence.exists())
            self.assertNotIn(str(root / "live"), Path("/proc/self/mountinfo").read_text())
        finally:
            if process.poll() is None:
                process.kill()
                process.wait()

    def test_generated_cli_retains_every_checkpoint_and_missing_upper_fails(self):
        spec = json.loads(Path(os.environ["TMLEGACY_SEAM_MANIFEST"]).read_text())
        root = Path(spec["scratch"])
        root.mkdir(exist_ok=False)
        self.addCleanup(__import__("shutil").rmtree, root)
        trace = {"version": 1, "seed": 0, "fixture": {"kind": "repository", "files": 1, "file_bytes": 0, "commit": spec["fixture_commit"]},
                 "agents": [{"id": "agent-0", "operations": [{"op": "write", "path": spec["regular_path"], "content": "first"},
                    {"op": "write", "path": spec["regular_path"], "content": "second"}, {"op": "bash", "command": "true"}]},
                    {"id": "agent-1", "operations": [{"op": "read", "path": spec["regular_path"]}, {"op": "list", "path": "."}]}]}
        trace_path = root / "trace.json"
        trace_path.write_text(json.dumps(trace) + "\n")
        common = ["/usr/bin/setpriv", "--pdeathsig", "KILL", sys.executable, HERE / "launch.py",
                  "--binary", spec["binary"], "--binary-sha256", spec["binary_sha256"], "--trace", trace_path,
                  "--trace-sha256", hashlib.sha256(trace_path.read_bytes()).hexdigest(), "--repo", spec["fixture"],
                  "--lower", spec["lower"], "--live-root", root / "live", "--slots", "1"]
        replay = root / "replay.json"
        subprocess.run([*common, "--json-out", replay, "--evidence-out", root / "replay-launch.json"], check=True)
        row = json.loads(replay.read_text())
        self.assertEqual(row["errors"], 0)
        self.assertEqual(row["checkpoint_overlay_proofs"], 2)
        self.assertEqual(row["checkpoint_close_proof_errors"], 0)
        subprocess.run([*common, "--audit-only", "--replay-report", replay, "--json-out", root / "audit.json",
                        "--evidence-out", root / "audit-launch.json"], check=True)
        self.assertTrue(json.loads((root / "audit.json").read_text())["passed"])
        checkpoint = ".overlay-" + hashlib.sha256(b"checkpoint-agent-0").hexdigest()
        __import__("shutil").rmtree(root / "live" / checkpoint / "upper")
        result = subprocess.run([*common, "--audit-only", "--replay-report", replay, "--json-out", root / "bad-audit.json",
                                 "--evidence-out", root / "bad-launch.json"])
        self.assertNotEqual(result.returncode, 0)
        self.assertGreater(json.loads((root / "bad-audit.json").read_text())["errors"], 0)


if __name__ == "__main__":
    unittest.main()
