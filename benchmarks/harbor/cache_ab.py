"""Preregister, run and recompute paired Harbor cache experiments."""

from __future__ import annotations

import argparse
import hashlib
import json
import math
import os
import random
import statistics
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path

_COUNTS = {"hits": "cmdcache_hits", "verifications": "cmdcache_verifications",
           "exit_mismatches": "cmdcache_verify_exit_mismatches",
           "write_mismatches": "cmdcache_verify_write_mismatches",
           "output_mismatches": "cmdcache_verify_output_mismatches"}
_METRICS = ("wall_seconds", "agent_seconds", "passed", "tokens", "bash_calls")
_MODES = {"off": "off", "A": "shadow", "B": "live", "C": "live"}
_REPO = Path(__file__).resolve().parents[2]
_ENV_KEYS = ("DOCKER_HOST", "DOCKER_CONTEXT", "THREADMILL_CONTEXT_WINDOW", "THREADMILL_EXEC_SLOTS",
             "THREADMILL_MODEL_PROXY", "THREADMILL_BUILD_PROXY", "OPENAI_BASE_URL", "OPENAI_API_BASE")


def _environment() -> dict:
    # Pin non-secret launch settings; credentials are deliberately excluded.
    return {key: hashlib.sha256(os.environ.get(key, "").encode()).hexdigest() for key in _ENV_KEYS}


def audit(rows: list[dict]) -> dict:
    result = {"ok": False, "reasons": [], **dict.fromkeys(_COUNTS, 0)}
    if not rows:
        result["reasons"].append("missing A trials")
    for row in rows:
        snapshot = row.get("snapshot") or {}
        label = f"{row['task']}/{row['attempt']}"
        if row.get("error"):
            result["reasons"].append(f"{label}: trial error")
        if snapshot.get("exec_dependency_tracing") is not True:
            result["reasons"].append(f"{label}: tracing not active")
        for name, field in _COUNTS.items():
            value = snapshot.get(field)
            if isinstance(value, bool) or not isinstance(value, int) or value < 0:
                result["reasons"].append(f"{label}: missing/invalid {field}")
            else:
                result[name] += value
        if snapshot.get("cmdcache_verifications") != snapshot.get("cmdcache_hits"):
            result["reasons"].append(f"{label}: A must verify every hit")
    for kind in ("exit_mismatches", "write_mismatches"):
        if result[kind]:
            result["reasons"].append(f"{kind}: {result[kind]}")
    if result["verifications"] == 0:
        result["reasons"].append("insufficient audit: zero verified hits")
    result["ok"] = not result["reasons"]
    return result


def _percentile(values: list[float], fraction: float) -> float:
    ordered = sorted(values)
    position = (len(ordered) - 1) * fraction
    lower = math.floor(position)
    upper = math.ceil(position)
    return ordered[lower] + (ordered[upper] - ordered[lower]) * (position - lower)


def _stats(rows: list[dict], metric: str, *, seed: int, bootstrap: int) -> dict:
    clusters = {}
    for row in rows:
        clusters.setdefault(row["task"], []).append(row[metric])
    values = [value for cluster in clusters.values() for value in cluster]
    rng = random.Random(seed)
    draws = []
    for _ in range(bootstrap):
        sample = [value for cluster in rng.choices(list(clusters.values()), k=len(clusters))
                  for value in cluster]
        draws.append({"mean": statistics.mean(sample), **{
            f"p{int(q * 100)}": _percentile(sample, q) for q in (.5, .9, .95)}})
    point = {"mean": statistics.mean(values), **{
        f"p{int(q * 100)}": _percentile(values, q) for q in (.5, .9, .95)}}
    return {**point, "ci95": {key: [_percentile([draw[key] for draw in draws], .025),
                                    _percentile([draw[key] for draw in draws], .975)]
                              for key in point}}


def summarize(rows: list[dict], *, seed: int = 20261002, bootstrap: int = 2000) -> dict:
    groups = sorted({row["group"] for row in rows})
    indexed = {(row["group"], row["task"], row["attempt"]): row for row in rows}
    if len(indexed) != len(rows):
        raise ValueError("duplicate task/attempt/group trials")
    report = {"bootstrap": {"seed": seed, "repetitions": bootstrap,
                            "unit": "task", "method": "paired percentile, 95%"},
              "groups": {}, "paired": {},
              "shadow_audit": audit([row for row in rows if row["group"] == "A"])}
    for group in groups:
        selected = [row for row in rows if row["group"] == group]
        report["groups"][group] = {"trials": len(selected), "tasks": len({r["task"] for r in selected}),
                                  "errors": sum(bool(r.get("error")) for r in selected),
                                  **{metric: _stats(selected, metric, seed=seed, bootstrap=bootstrap)
                                     for metric in _METRICS}}
    for treatment, control in (("A", "off"), ("B", "A"), ("C", "B")):
        pairs = [(row, indexed[(control, row["task"], row["attempt"])])
                 for row in rows if row["group"] == treatment
                 and (control, row["task"], row["attempt"]) in indexed]
        if not pairs:
            continue
        comparison = {}
        for metric in _METRICS:
            deltas = [{"task": left["task"], metric: left[metric] - right[metric]}
                      for left, right in pairs]
            stats = _stats(deltas, metric, seed=seed, bootstrap=bootstrap)
            comparison[metric] = {"pairs": len(pairs), "tasks": len({r["task"] for r in deltas}),
                                  "mean_delta": stats.pop("mean"),
                                  "ci95": stats["ci95"]["mean"],
                                  "percentiles": {key: value for key, value in stats.items() if key != "ci95"},
                                  "percentile_ci95": {key: value for key, value in stats["ci95"].items()
                                                      if key != "mean"}}
        report["paired"][f"{treatment}-{control}"] = comparison
    return report


def _sha(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def _tree_sha(root: Path) -> str:
    digest = hashlib.sha256()
    for path in sorted(root.rglob("*")):
        if path.is_file() or path.is_symlink():
            digest.update(str(path.relative_to(root)).encode() + b"\0")
            digest.update(str(path.lstat().st_mode).encode() + b"\0")
            digest.update(os.readlink(path).encode() if path.is_symlink() else path.read_bytes())
    return digest.hexdigest()


def _command(*args: str) -> str:
    return subprocess.run(args, check=True, capture_output=True, text=True).stdout.strip()


def _source() -> dict:
    names = subprocess.run(["git", "-C", str(_REPO), "ls-files", "-z", "--cached", "--others",
                            "--exclude-standard"], check=True, capture_output=True).stdout.split(b"\0")
    digest = hashlib.sha256()
    for name in sorted(filter(None, names)):
        path = _REPO / os.fsdecode(name)
        digest.update(name + b"\0")
        if path.is_file():
            digest.update(path.read_bytes())
    return {"commit": _command("git", "-C", str(_REPO), "rev-parse", "HEAD"),
            "dirty": bool(_command("git", "-C", str(_REPO), "status", "--porcelain")),
            "tree_sha256": digest.hexdigest()}


def _plan_digest(plan: dict) -> str:
    return hashlib.sha256(json.dumps({k: v for k, v in plan.items() if k != "sha256"},
                                    sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def prepare(args) -> None:
    from harbor.environments.definition import environment_content_hash
    from harbor.models.task.task import Task
    import yaml

    output = args.output.resolve()
    if (output / "plan.json").exists():
        raise ValueError("plan.json already exists; use a new output directory")
    count = len(args.task)
    if (args.phase == "pilot" and count != 3) or (args.phase == "full" and not 10 <= count <= 15):
        raise ValueError("pilot requires 3 tasks; full requires 10–15 tasks")
    if not args.reward_key or not math.isfinite(args.pass_value) or args.pass_value <= 0:
        raise ValueError("pass criterion requires a reward key and a finite positive pass value")
    doctor = json.loads(args.doctor.read_text())
    if doctor.get("ok") is not True:
        raise ValueError("doctor did not pass; no model trials are permitted")
    runtime_version = _command(args.runtime, "--version")
    for name, value in (("binary_sha256", _sha(args.binary)), ("tracer_sha256", _sha(args.tracer)),
                        ("workspace", args.workspace), ("runtime_version", runtime_version)):
        if doctor.get(name) != value:
            raise ValueError(f"doctor does not match {name}; repeat preflight for these artifacts")
    source = _source()
    if args.phase == "full" and source["dirty"]:
        raise ValueError("formal measurements require a clean, fixed commit")
    tasks = []
    for path in args.task:
        path = path.resolve()
        task = Task(path)
        image = task.config.environment.docker_image or (
            "hb__" + environment_content_hash(task.paths.environment_dir))
        inspected = json.loads(_command("docker", "image", "inspect", image))[0]
        tasks.append({"id": task.name, "path": str(path), "sha256": _tree_sha(path),
                      "image": image, "image_id": inspected["Id"]})
    if len({t["id"] for t in tasks}) != count:
        raise ValueError("task names must be unique")
    groups = ["off", "A", "B"] + (["C"] if args.hints_config else [])
    attempts = 1 if args.phase == "pilot" else 5
    jobs = []
    rng = random.Random(args.seed)
    for group in groups:
        group_jobs = [{"group": group, "task": task["id"], "attempt": attempt,
                       "job": f"{group}-task{index + 1:02d}-r{attempt}"}
                      for index, task in enumerate(tasks[:min(count, args.off_tasks)] if group == "off" else tasks)
                      for attempt in range(1, attempts + 1)]
        rng.shuffle(group_jobs)
        jobs.extend(group_jobs)
    config_paths = {name: {"path": str(path.resolve()), "sha256": _sha(path)}
                    for name, path in (("base", args.config), ("hints", args.hints_config)) if path}
    if args.hints_config:
        hint_config = json.loads(args.hints_config.read_text())
        if set(hint_config) != {"exec"} or set(hint_config["exec"]) != {"cache"} or (
                {"enabled", "verify_sample_rate"} & set(hint_config["exec"]["cache"])):
            raise ValueError("C config must contain only exec.cache hint settings")
        base = yaml.safe_load(args.config.read_text()) if args.config else {}
        base = base or {}
        base_exec = base.get("exec", {})
        combined = {**base, "exec": {**base_exec, "cache": {
            **base_exec.get("cache", {}), **hint_config["exec"]["cache"]}}}
        output.mkdir(parents=True, exist_ok=True)
        path = output / "C-config.json"
        path.write_text(json.dumps(combined, indent=2) + "\n")
        config_paths["C"] = {"path": str(path), "sha256": _sha(path)}
    forbidden = {"--config", "-c", "--model", "-m", "--agent", "-a", "--agent-kwarg", "--ak",
                 "--agent-import-path", "--verifier", "--verifier-import-path", "--verifier-kwarg",
                 "--install-only", "--extra-instruction", "--extra-instruction-path",
                 "--path", "-p", "--dataset", "-d", "--task", "-t", "--repo", "--job-name",
                 "--task-git-url", "--task-git-commit", "--exclude-task-name", "--task-name",
                 "--n-attempts", "-k", "--jobs-dir", "-o", "--print-config", "--disable-verification"}
    if any(arg.split("=", 1)[0] in forbidden for arg in args.harbor_arg):
        raise ValueError("Harbor args cannot override preregistered task/model/agent/group or disable grading")
    plan = {"schema": 1, "registered_utc": datetime.now(timezone.utc).isoformat(),
            "phase": args.phase, "source": source, "model": args.model, "tasks": tasks,
            "attempts": attempts, "off_tasks": args.off_tasks,
            "groups": groups, "jobs": jobs, "seed": args.seed,
            "seed_scope": "task order and bootstrap; model sampling follows the base configuration",
            "bootstrap_repetitions": 2000, "doctor": doctor,
            "runtime": args.runtime, "runtime_version": runtime_version,
            "binary": {"path": str(args.binary.resolve()), "sha256": _sha(args.binary)},
            "tracer": {"path": str(args.tracer.resolve()), "sha256": _sha(args.tracer)},
            "configs": config_paths, "harbor_args": args.harbor_arg,
            "pass_criterion": {"reward_key": args.reward_key, "minimum": args.pass_value},
            "workspace": args.workspace, "environment_sha256": _environment(),
            "predictions": {"go": "native Go result caching may reduce cmdcache benefit",
                            "python": "test result reuse should save more time than Go",
                            "B": "positive paired time savings without a pass-rate decrease"},
            "stop_rule": "do not enter B/C after A exit/write mismatch, missing audit data, or zero verified hits"}
    plan["sha256"] = _plan_digest(plan)
    output.mkdir(parents=True, exist_ok=True)
    (output / "plan.json").write_text(json.dumps(plan, indent=2, ensure_ascii=False) + "\n")
    print(output / "plan.json")


def _read_plan(root: Path) -> dict:
    plan = json.loads((root / "plan.json").read_text())
    if plan.get("schema") != 1 or plan.get("sha256") != _plan_digest(plan):
        raise ValueError("invalid or modified preregistration")
    count = len(plan["tasks"])
    if plan["phase"] == "full":
        if not 10 <= count <= 15 or plan["attempts"] != 5:
            raise ValueError("formal measurements require 10–15 tasks × 5 attempts")
    elif plan["phase"] != "pilot" or count != 3 or plan["attempts"] != 1:
        raise ValueError("pilot requires 3 tasks × 1 attempt")
    if plan["groups"] not in (["off", "A", "B"], ["off", "A", "B", "C"]):
        raise ValueError("invalid group matrix")
    expected = {(group, task["id"], attempt)
                for group in plan["groups"]
                for task in (plan["tasks"][:plan["off_tasks"]] if group == "off" else plan["tasks"])
                for attempt in range(1, plan["attempts"] + 1)}
    actual = {(job["group"], job["task"], job["attempt"]) for job in plan["jobs"]}
    if len(actual) != len(plan["jobs"]) or actual != expected:
        raise ValueError("missing or duplicate preregistered pairs")
    return plan


def _verify_frozen(plan: dict) -> None:
    if plan["source"] != _source():
        raise ValueError("source changed after preregistration")
    if _command(plan["runtime"], "--version") != plan["runtime_version"]:
        raise ValueError("Harbor/Pier runtime changed")
    if _environment() != plan["environment_sha256"]:
        raise ValueError("launch environment changed after preregistration")
    for artifact in [plan["binary"], plan["tracer"], *plan["configs"].values()]:
        if _sha(Path(artifact["path"])) != artifact["sha256"]:
            raise ValueError(f"artifact changed: {artifact['path']}")
    info = json.loads(_command("docker", "info", "--format", "{{json .}}"))
    if info["Driver"] != "btrfs" or any(info[key] != plan["doctor"]["docker"][key]
                                        for key in ("ID", "DockerRootDir", "ServerVersion")):
        raise ValueError("Docker environment changed or driver is not btrfs")
    for task in plan["tasks"]:
        if _tree_sha(Path(task["path"])) != task["sha256"]:
            raise ValueError(f"task changed: {task['id']}")
        image = json.loads(_command("docker", "image", "inspect", task["image"]))[0]
        if image["Id"] != task["image_id"]:
            raise ValueError(f"task image changed: {task['id']}")


def _duration(result: dict, section: str | None = None) -> float:
    value = result if section is None else result.get(section) or {}
    begin = datetime.fromisoformat(value["started_at"].replace("Z", "+00:00"))
    end = datetime.fromisoformat(value["finished_at"].replace("Z", "+00:00"))
    duration = (end - begin).total_seconds()
    if not math.isfinite(duration) or duration < 0:
        raise ValueError(f"invalid timing: {section}")
    return duration


def _row(root: Path, job: dict, plan: dict) -> dict:
    candidates = list((root / "jobs" / job["job"]).glob("*/result.json"))
    if len(candidates) != 1:
        raise ValueError(f"{job['job']}: expected one trial result, got {len(candidates)}")
    path = candidates[0]
    result = json.loads(path.read_text())
    if result["task_name"] != job["task"]:
        raise ValueError(f"{job['job']}: task identity changed")
    if result["config"]["agent"]["model_name"] != plan["model"]:
        raise ValueError(f"{job['job']}: model changed")
    image = json.loads((root / "environment" / f"{job['job']}.json").read_text())
    task = next(task for task in plan["tasks"] if task["id"] == job["task"])
    if image.get("before") != task["image_id"] or image.get("after") != task["image_id"]:
        raise ValueError(f"{job['job']}: task image changed or missing image evidence")
    mode = result["config"]["agent"]["kwargs"].get("cache_mode")
    if mode != _MODES[job["group"]]:
        raise ValueError(f"{job['job']}: cache configuration changed")
    metadata = (result.get("agent_result") or {}).get("metadata") or {}
    snapshot = metadata.get("threadmill_runtime_snapshot")
    if not snapshot:
        raise ValueError(f"{job['job']}: missing runtime snapshot")
    for field in _COUNTS.values():
        if field not in snapshot:
            raise ValueError(f"{job['job']}: missing {field}")
    preflight = json.loads((path.parent / "agent" / "threadmill" / "preflight.json").read_text())
    enabled = job["group"] != "off"
    if preflight.get("exec_dependency_tracing") is not True or (
            preflight.get("exec_dependency_tracing_enabled") is not enabled):
        raise ValueError(f"{job['job']}: missing tracing preflight")
    rewards = (result.get("verifier_result") or {}).get("rewards")
    if not rewards:
        raise ValueError(f"{job['job']}: missing verifier score")
    criterion = plan["pass_criterion"]
    score = float(rewards[criterion["reward_key"]])
    if not math.isfinite(score):
        raise ValueError(f"{job['job']}: invalid verifier score")
    tokens = snapshot["input_tokens"] + snapshot["tokens"] + snapshot.get("memory_input_tokens", 0) + snapshot.get("memory_ops_tokens", 0)
    return {**job, "trial_result": str(path), "task_checksum": result["task_checksum"],
            "wall_seconds": _duration(result), "agent_seconds": _duration(result, "agent_execution"),
            "passed": int(score >= criterion["minimum"]),
            "rewards": rewards, "tokens": tokens, "bash_calls": snapshot["exec_requests"],
            "bash_calls_source": "exec_requests (execution calls)",
            "snapshot": snapshot, "preflight": preflight, "environment": image,
            "error": result.get("exception_info")}


def _load_rows(root: Path, plan: dict, group: str | None = None) -> tuple[list[dict], list[str]]:
    rows, missing = [], []
    for job in plan["jobs"]:
        if group and job["group"] != group:
            continue
        try:
            rows.append(_row(root, job, plan))
        except (OSError, KeyError, TypeError, ValueError) as error:
            missing.append(f"{job['job']}: {error}")
    return rows, missing


def report(root: Path) -> dict:
    plan = _read_plan(root)
    rows, missing = _load_rows(root, plan)
    result = summarize(rows, seed=plan["seed"], bootstrap=plan["bootstrap_repetitions"])
    result.update({"phase": plan["phase"], "plan_sha256": plan["sha256"], "missing": missing,
                   "pass_criterion": plan["pass_criterion"],
                   "complete": not missing, "formal": plan["phase"] == "full" and not missing})
    if any(row.get("error") for row in rows):
        result["formal"] = False
    if not result["shadow_audit"]["ok"]:
        result["formal"] = False
    checksums = {}
    for row in rows:
        checksums.setdefault(row["task"], set()).add(row["task_checksum"])
    if any(len(values) != 1 for values in checksums.values()):
        result["missing"].append("paired task checksums differ")
        result["complete"] = result["formal"] = False
    (root / "observations.json").write_text(json.dumps(rows, indent=2, ensure_ascii=False) + "\n")
    (root / "report.json").write_text(json.dumps(result, indent=2, ensure_ascii=False) + "\n")
    return result


def run(root: Path, group: str | None = None) -> None:
    plan = _read_plan(root)
    _verify_frozen(plan)
    for selected in ([group] if group else plan["groups"]):
        if selected not in plan["groups"]:
            raise ValueError(f"group {selected} was not preregistered")
        if selected in ("B", "C"):
            shadows, missing = _load_rows(root, plan, "A")
            gate = audit(shadows)
            if missing or not gate["ok"]:
                report(root)
                raise ValueError("A blocks B/C: " + "; ".join(missing + gate["reasons"]))
        for job in [job for job in plan["jobs"] if job["group"] == selected]:
            _verify_frozen(plan)
            job_root = root / "jobs" / job["job"]
            if job_root.exists():
                row = _row(root, job, plan)
            else:
                task = next(task for task in plan["tasks"] if task["id"] == job["task"])
                env = {**os.environ, "THREADMILL_BINARY": plan["binary"]["path"],
                       "THREADMILL_STRACE_BINARY": plan["tracer"]["path"],
                       "THREADMILL_HARBOR_BIN": plan["runtime"], "THREADMILL_CACHE_MODE": _MODES[selected],
                       "THREADMILL_BENCHMARK_JOBS": str(root / "jobs"),
                       "THREADMILL_BENCH_WORKSPACE": plan["workspace"]}
                config = plan["configs"].get("C" if selected == "C" else "base")
                if config:
                    env["THREADMILL_BENCH_CONFIG"] = config["path"]
                else:
                    env.pop("THREADMILL_BENCH_CONFIG", None)
                image = {"before": json.loads(_command("docker", "image", "inspect", task["image"]))[0]["Id"]}
                if image["before"] != task["image_id"]:
                    raise ValueError(f"task image changed: {task['id']}")
                try:
                    subprocess.run([str(_REPO / "benchmarks/harbor/bench"), "run-path", task["path"],
                                    plan["model"], "--job-name", job["job"], "--n-attempts", "1",
                                    *plan["harbor_args"]], env=env, check=True)
                finally:
                    image["after"] = json.loads(_command("docker", "image", "inspect", task["image"]))[0]["Id"]
                    (root / "environment").mkdir(exist_ok=True)
                    (root / "environment" / f"{job['job']}.json").write_text(json.dumps(image) + "\n")
                row = _row(root, job, plan)
            if selected == "A":
                gate = audit([row])
                failures = [reason for reason in gate["reasons"] if "zero verified hits" not in reason]
                if failures:
                    report(root)
                    raise ValueError("A stopped: " + "; ".join(failures))
        report(root)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    prep = commands.add_parser("prepare")
    prep.add_argument("--output", type=Path, required=True)
    prep.add_argument("--phase", choices=("pilot", "full"), default="pilot")
    prep.add_argument("--task", type=Path, action="append", required=True)
    prep.add_argument("--model", required=True)
    prep.add_argument("--reward-key", default="reward", help="Grader reward key defining a pass")
    prep.add_argument("--pass-value", type=float, default=1.0, help="Minimum value for a pass, fixed before trials")
    prep.add_argument("--doctor", type=Path, required=True)
    prep.add_argument("--binary", type=Path, required=True)
    prep.add_argument("--tracer", type=Path, required=True)
    prep.add_argument("--runtime", default=os.environ.get("THREADMILL_HARBOR_BIN", "harbor"))
    prep.add_argument("--workspace", default="/workspace/repo")
    prep.add_argument("--config", type=Path)
    prep.add_argument("--hints-config", type=Path)
    prep.add_argument("--off-tasks", type=int, default=3, choices=range(1, 4))
    prep.add_argument("--seed", type=int, default=20261002)
    prep.add_argument("--harbor-arg", action="append", default=[])
    for command in ("run", "report", "audit"):
        cmd = commands.add_parser(command)
        cmd.add_argument("root", type=Path)
        if command == "run":
            cmd.add_argument("--group", choices=_MODES)
    args = parser.parse_args()
    try:
        if args.command == "prepare":
            prepare(args)
        elif args.command == "run":
            run(args.root.resolve(), args.group)
        else:
            result = report(args.root.resolve())
            print(json.dumps(result["shadow_audit"] if args.command == "audit" else result,
                             indent=2, ensure_ascii=False))
            if args.command == "audit":
                shadows, missing = _load_rows(args.root.resolve(), _read_plan(args.root.resolve()), "A")
                return 0 if not missing and audit(shadows)["ok"] else 1
            return 0 if result["complete"] and result["shadow_audit"]["ok"] and not any(
                group["errors"] for group in result["groups"].values()) else 1
    except (OSError, ValueError, KeyError, TypeError, subprocess.CalledProcessError) as error:
        print(f"cache-ab: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
