import argparse
import hashlib
import io
import json
import subprocess
import tempfile
import unittest
from pathlib import Path
from contextlib import redirect_stdout
from unittest.mock import patch

from benchmarks.harbor.cache_ab import _plan_digest, audit, main, prepare, report, run, summarize


def pilot(directory, *, write_mismatches=0, rewards_by_task=None):
    root = Path(directory)
    binary, tracer, doctor = (root / name for name in ("threadmill", "strace", "doctor.json"))
    binary.write_bytes(b"pinned binary")
    tracer.write_bytes(b"pinned tracer")
    docker = {"ID": "isolated", "Driver": "btrfs", "DockerRootDir": "/dedicated/data",
              "ServerVersion": "29.1.3"}
    doctor.write_text(json.dumps({"ok": True, "docker": docker, "runtime_version": "0.22.0",
                                 "workspace": "/workspace/repo",
                                 "binary_sha256": hashlib.sha256(binary.read_bytes()).hexdigest(),
                                 "tracer_sha256": hashlib.sha256(tracer.read_bytes()).hexdigest()}))
    tasks = []
    for name in ("one", "two", "three"):
        task = root / "tasks" / name
        (task / "environment").mkdir(parents=True)
        (task / "tests").mkdir()
        (task / "instruction.md").write_text("Run the tests.\n")
        (task / "task.toml").write_text('[environment]\ndocker_image = "test/image"\n')
        (task / "tests" / "test.sh").write_text("exit 0\n")
        tasks.append(task)
    args = argparse.Namespace(output=root / "experiment", phase="pilot", task=tasks,
                              doctor=doctor, binary=binary, tracer=tracer,
                              bwrap=None, exec_backend="external",
                              hints_config=None, config=None, off_tasks=3, seed=1,
                              harbor_arg=[], model="provider/model", runtime="test-harbor",
                              workspace="/workspace/repo", reward_key="reward", pass_value=1.0)
    real_run = subprocess.run
    launched = []

    def process(command, **kwargs):
        if command[0] == "test-harbor":
            return subprocess.CompletedProcess(command, 0, stdout="0.22.0\n")
        if command[0] == "docker":
            value = docker if command[1] == "info" else [{"Id": "sha256:fixed"}]
            return subprocess.CompletedProcess(command, 0, stdout=json.dumps(value))
        if command[0] == "git":
            if "ls-files" in command:
                value = b""
            else:
                value = "fixture-commit" if "rev-parse" in command else ""
            return subprocess.CompletedProcess(command, 0, stdout=value)
        if Path(command[0]).name == "bench":
            env = kwargs["env"]
            mode = env["THREADMILL_CACHE_MODE"]
            name = Path(command[2]).name
            job = command[command.index("--job-name") + 1]
            trial = Path(env["THREADMILL_BENCHMARK_JOBS"]) / job / "trial"
            logs = trial / "agent" / "threadmill"
            logs.mkdir(parents=True)
            preflight = {"exec_dependency_tracing": True,
                         "exec_dependency_tracing_enabled": mode != "off"}
            (logs / "preflight.json").write_text(json.dumps(preflight))
            snapshot = row("A", name, 1, 1)["snapshot"]
            snapshot.update(input_tokens=10, tokens=4, memory_input_tokens=2,
                            memory_ops_tokens=4, exec_requests=3,
                            exec_dependency_tracing=mode != "off")
            if mode == "off":
                snapshot.update(cmdcache_hits=0, cmdcache_verifications=0)
            if mode == "shadow":
                snapshot["cmdcache_verify_write_mismatches"] = write_mismatches
            seconds = {"off": 10, "shadow": 8, "live": 6}[mode]
            timing = {"started_at": "2026-10-02T00:00:00Z",
                      "finished_at": f"2026-10-02T00:00:{seconds:02d}Z"}
            result = {**timing, "task_name": name, "task_checksum": "fixed-task-" + name,
                      "config": {"agent": {"model_name": args.model, "kwargs": {"cache_mode": mode}}},
                      "agent_result": {"metadata": {"threadmill_runtime_snapshot": snapshot}},
                      "verifier_result": {"rewards": (rewards_by_task or {}).get(name, {"reward": 1})},
                      "agent_execution": timing, "exception_info": None}
            (trial / "result.json").write_text(json.dumps(result))
            launched.append(mode)
            return subprocess.CompletedProcess(command, 0)
        return real_run(command, **kwargs)

    return args, process, launched


def row(group, task, attempt, wall, **counts):
    snapshot = {
        "exec_dependency_tracing": True,
        "cmdcache_hits": 2,
        "cmdcache_verifications": 2,
        "cmdcache_verify_exit_mismatches": 0,
        "cmdcache_verify_write_mismatches": 0,
        "cmdcache_verify_output_mismatches": 0,
    }
    snapshot.update(counts)
    return {"group": group, "task": task, "attempt": attempt,
            "wall_seconds": wall, "agent_seconds": wall - 1,
            "passed": 1, "tokens": 10, "bash_calls": 3,
            "snapshot": snapshot, "error": None}


class CacheABTest(unittest.TestCase):
    def test_preregistration_rejects_a_different_bwrap_admission(self):
        for mismatch in ("exec_backend", "runtime_preflight", "bwrap_sha256"):
            with self.subTest(mismatch=mismatch), tempfile.TemporaryDirectory() as directory:
                args, process, _ = pilot(directory)
                args.exec_backend, args.bwrap = "bwrap", Path(directory) / "bwrap"
                args.bwrap.write_bytes(b"pinned bwrap")
                doctor = json.loads(args.doctor.read_text())
                doctor.update(exec_backend="bwrap", runtime_preflight={"exec_sandbox_backend": "bwrap"},
                              bwrap_sha256=hashlib.sha256(args.bwrap.read_bytes()).hexdigest())
                doctor[mismatch] = {} if mismatch == "runtime_preflight" else "different"
                args.doctor.write_text(json.dumps(doctor))
                with patch("benchmarks.harbor.cache_ab.subprocess.run", side_effect=process):
                    with self.assertRaisesRegex(ValueError, "doctor.*(backend|bwrap)"):
                        prepare(args)
                self.assertFalse((args.output / "plan.json").exists())

    def test_registered_bwrap_is_forwarded_and_cannot_change_before_a_trial(self):
        with tempfile.TemporaryDirectory() as directory:
            args, process, launched = pilot(directory)
            args.exec_backend, args.bwrap = "bwrap", Path(directory) / "bwrap"
            args.bwrap.write_bytes(b"pinned bwrap")
            doctor = json.loads(args.doctor.read_text())
            doctor.update(exec_backend="bwrap", runtime_preflight={"exec_sandbox_backend": "bwrap"},
                          bwrap_sha256=hashlib.sha256(args.bwrap.read_bytes()).hexdigest())
            args.doctor.write_text(json.dumps(doctor))

            def checked_process(command, **kwargs):
                if Path(command[0]).name == "bench":
                    self.assertEqual(kwargs["env"]["THREADMILL_BENCH_EXEC_BACKEND"], "bwrap")
                    self.assertEqual(kwargs["env"]["THREADMILL_BWRAP_BINARY"], str(args.bwrap))
                return process(command, **kwargs)

            with patch("benchmarks.harbor.cache_ab.subprocess.run", side_effect=checked_process):
                prepare(args)
                run(args.output, "off")
                args.bwrap.write_bytes(b"changed bwrap")
                with self.assertRaisesRegex(ValueError, "artifact changed.*bwrap"):
                    run(args.output, "A")
            self.assertEqual(launched, ["off"] * 3)

    def test_preregistration_rejects_doctor_for_another_binary(self):
        with tempfile.TemporaryDirectory() as directory:
            args, process, _ = pilot(directory)
            args.binary.write_bytes(b"a different binary")
            with patch("benchmarks.harbor.cache_ab.subprocess.run", side_effect=process):
                with self.assertRaisesRegex(ValueError, "doctor.*binary"):
                    prepare(args)
            self.assertFalse((args.output / "plan.json").exists())

    def test_preregistration_rejects_grader_and_task_overrides(self):
        for override in ("--verifier=other:Verifier", "--agent-import-path=other:Agent", "--install-only"):
            with self.subTest(override=override), tempfile.TemporaryDirectory() as directory:
                args, process, _ = pilot(directory)
                args.harbor_arg = [override]
                with patch("benchmarks.harbor.cache_ab.subprocess.run", side_effect=process):
                    with self.assertRaisesRegex(ValueError, "Harbor args cannot override"):
                        prepare(args)

    def test_pass_criterion_uses_pinned_score_and_keeps_auxiliary_zero_fields(self):
        scores = {"one": {"score": 100, "valid": 1, "stage2_failed": 0},
                  "two": {"score": 80, "valid": 1, "stage2_failed": 0},
                  "three": {"score": 0, "valid": 0, "stage2_failed": 1}}
        with tempfile.TemporaryDirectory() as directory:
            args, process, _ = pilot(directory, rewards_by_task=scores)
            args.reward_key, args.pass_value = "score", 100.0
            with patch("benchmarks.harbor.cache_ab.subprocess.run", side_effect=process):
                prepare(args)
                run(args.output, "off")
            rows = json.loads((args.output / "observations.json").read_text())
            self.assertEqual({row["task"]: row["passed"] for row in rows},
                             {"one": 1, "two": 0, "three": 0})
            self.assertEqual({row["task"]: row["rewards"] for row in rows}, scores)

    def test_registered_pilot_runs_paired_phases_and_preserves_missing_results(self):
        with tempfile.TemporaryDirectory() as directory:
            args, process, launched = pilot(directory)
            with patch("benchmarks.harbor.cache_ab.subprocess.run", side_effect=process):
                prepare(args)
                run(args.output)
            result = report(args.output)
            self.assertEqual(launched, ["off"] * 3 + ["shadow"] * 3 + ["live"] * 3)
            self.assertTrue(result["complete"])
            self.assertFalse(result["formal"])
            self.assertTrue(result["shadow_audit"]["ok"])
            self.assertEqual(result["paired"]["B-A"]["wall_seconds"]["mean_delta"], -2)
            self.assertEqual(result["groups"]["B"]["tokens"]["mean"], 20)
            self.assertEqual(len(json.loads((args.output / "observations.json").read_text())), 9)
            trial = next((args.output / "jobs").glob("B-*/*/result.json"))
            failed = json.loads(trial.read_text())
            failed["exception_info"] = {"exception_type": "TaskError"}
            trial.write_text(json.dumps(failed))
            with patch("sys.argv", ["cache-ab", "report", str(args.output)]), redirect_stdout(io.StringIO()):
                self.assertEqual(main(), 1)
            trial.unlink()
            incomplete = report(args.output)
            self.assertFalse(incomplete["complete"])
            self.assertEqual(len(incomplete["missing"]), 1)

    def test_shadow_write_mismatch_stops_before_next_trial_or_live_phase(self):
        with tempfile.TemporaryDirectory() as directory:
            args, process, launched = pilot(directory, write_mismatches=1)
            with patch("benchmarks.harbor.cache_ab.subprocess.run", side_effect=process):
                prepare(args)
                with self.assertRaisesRegex(ValueError, "A stopped.*write_mismatches"):
                    run(args.output)
                with self.assertRaisesRegex(ValueError, "A blocks B"):
                    run(args.output, "B")
            self.assertEqual(launched, ["off"] * 3 + ["shadow"])
            self.assertEqual(report(args.output)["shadow_audit"]["write_mismatches"], 1)

    def test_run_rejects_artifact_change_between_trials(self):
        with tempfile.TemporaryDirectory() as directory:
            args, process, launched = pilot(directory)

            def changing(command, **kwargs):
                result = process(command, **kwargs)
                if launched:
                    args.binary.write_bytes(b"binary changed after first trial")
                return result

            with patch("benchmarks.harbor.cache_ab.subprocess.run", side_effect=changing):
                prepare(args)
                with self.assertRaisesRegex(ValueError, "artifact changed"):
                    run(args.output)
            self.assertEqual(launched, ["off"])

    def test_report_rejects_formal_manifest_with_two_tasks(self):
        plan = {"schema": 1, "phase": "full", "tasks": [{"id": "one"}, {"id": "two"}],
                "jobs": [], "groups": ["off", "A", "B"], "attempts": 5,
                "off_tasks": 2, "seed": 1, "bootstrap_repetitions": 100}
        plan["sha256"] = _plan_digest(plan)
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "plan.json").write_text(json.dumps(plan))
            with self.assertRaisesRegex(ValueError, "10.*15"):
                report(root)

    def test_shadow_audit_stops_on_exit_and_missing_write_evidence(self):
        self.assertFalse(audit([row("A", "t", 1, 10,
                                  cmdcache_verify_exit_mismatches=1)])["ok"])
        incomplete = row("A", "t", 1, 10)
        del incomplete["snapshot"]["cmdcache_verify_write_mismatches"]
        self.assertFalse(audit([incomplete])["ok"])
        self.assertFalse(audit([row("A", "t", 1, 10,
                                  cmdcache_hits=0, cmdcache_verifications=0)])["ok"])
        output_only = audit([row("A", "t", 1, 10,
                                cmdcache_verify_output_mismatches=3)])
        self.assertTrue(output_only["ok"])
        self.assertEqual(output_only["output_mismatches"], 3)

    def test_summary_pairs_tasks_and_bootstraps_task_clusters(self):
        rows = [row("A", "one", i, 10) for i in (1, 2)]
        rows += [row("A", "two", i, 20) for i in (1, 2)]
        rows += [row("B", "two", i, 18) for i in (2, 1)]
        rows += [row("B", "one", i, 8) for i in (2, 1)]
        report = summarize(rows, seed=1, bootstrap=100)
        self.assertEqual(report["groups"]["A"]["wall_seconds"]["p50"], 15)
        self.assertEqual(report["groups"]["A"]["wall_seconds"]["p90"], 20)
        self.assertEqual(report["groups"]["A"]["wall_seconds"]["p95"], 20)
        paired = report["paired"]["B-A"]["wall_seconds"]
        self.assertEqual(paired["mean_delta"], -2)
        self.assertEqual(paired["ci95"], [-2, -2])
        self.assertEqual(paired["pairs"], 4)
        self.assertEqual(paired["tasks"], 2)


if __name__ == "__main__":
    unittest.main()
