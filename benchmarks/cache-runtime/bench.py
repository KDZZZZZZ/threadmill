#!/usr/bin/env python3
"""Real Go/pytest commands with a deterministic cache oracle, without models."""

import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import random
import shlex
import shutil
import statistics
import subprocess
import sys

HERE = Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location("pi_runtime_helpers", HERE.parent / "pi-runtime/bench.py")
runtime = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(runtime)


def script_trace(language, commit, agents, seed, edit_probability, baseline, changed, python, go="go"):
    if agents < 1 or not 0 <= edit_probability <= 1:
        raise ValueError("agents must be positive and edit probability between zero and one")
    if language == "go":
        prefix = "GOTOOLCHAIN=local " + shlex.quote(go)
        commands = [prefix + " test ./...", prefix + " test ./pricing", prefix + " vet ./..."]
        target = "pricing/price.go"
    else:
        prefix = f"PYTEST_DISABLE_PLUGIN_AUTOLOAD=1 PYTHONDONTWRITEBYTECODE=1 {shlex.quote(python)} -m pytest -p no:cacheprovider -q"
        commands, target = [prefix, prefix + " tests/test_pricing.py"], "pricing.py"
    rng, seen, records = random.Random(seed), set(), []
    for index in range(agents):
        modified = rng.random() < edit_probability
        content = changed if modified else baseline
        operations = [{"op": "write", "path": target, "content": content}]
        for command in commands:
            key = (command, content)
            for _ in range(2):
                operations.append({"op": "bash", "command": command, "expected_exit": 0, "expected_cache": key in seen})
                seen.add(key)
        records.append({"id": f"agent-{index}", "operations": operations})
    return {"version": 1, "seed": seed, "fixture": {"kind": "repository", "files": 0, "file_bytes": 0, "commit": commit},
            "serial": True, "agents": records}


def make_fixture(root, language, python, go):
    repo = root / f"{language}-fixture"
    if repo.exists():
        raise RuntimeError(f"refusing to overwrite {repo}")
    if language == "go":
        shutil.copytree(runtime.PROJECT / "test/crash-recovery-project", repo)
        fixes = {
            "pricing/price.go": [("subtotal < 0", "subtotal <= 0"), ("subtotal > 10_000", "subtotal >= 10_000")],
            "inventory/reserve.go": [("requested < 0", "requested <= 0"), ("requested >= available", "requested > available")],
            "shipping/fee.go": [("amount > 9_000", "amount >= 9_000")],
            "checkout/quote.go": [("shipping.Fee(subtotal)", "shipping.Fee(items)"), ("Total: subtotal + delivery", "Total: items + delivery")],
        }
        for relative, substitutions in fixes.items():
            path = repo / relative
            content = path.read_text()
            for old, new in substitutions:
                if old not in content:
                    raise RuntimeError(f"fixture source drift: {relative} missing {old}")
                content = content.replace(old, new)
            path.write_text(content)
        runtime.run([go, "test", "./..."], cwd=repo, env={**os.environ, "GOTOOLCHAIN": "local"})
        target = repo / "pricing/price.go"
        baseline = target.read_text()
        # A source edit with identical expected assertions, exercising invalidation.
        changed = baseline.replace("subtotal * 90 / 100", "subtotal - subtotal/10")
    else:
        shutil.copytree(HERE / "fixture", repo, ignore=shutil.ignore_patterns("__pycache__", ".pytest_cache"))
        runtime.run([python, "-m", "pytest", "-p", "no:cacheprovider", "-q"], cwd=repo,
                    env={**os.environ, "PYTHONDONTWRITEBYTECODE": "1", "PYTEST_DISABLE_PLUGIN_AUTOLOAD": "1"})
        target, baseline = repo / "pricing.py", (repo / "pricing.py").read_text()
        changed = baseline.replace("subtotal * 90 // 100", "subtotal - subtotal // 10")
    # Ensure the two states actually differ and both have known passing results.
    if changed == baseline:
        raise RuntimeError("fixture edit did not change the source")
    target.write_text(changed)
    if language == "go":
        runtime.run([go, "test", "./..."], cwd=repo, env={**os.environ, "GOTOOLCHAIN": "local"})
    else:
        runtime.run([python, "-m", "pytest", "-p", "no:cacheprovider", "-q"], cwd=repo,
                    env={**os.environ, "PYTHONDONTWRITEBYTECODE": "1", "PYTEST_DISABLE_PLUGIN_AUTOLOAD": "1"})
    target.write_text(baseline)
    commit = runtime.commit_fixture(repo)
    return repo, commit, baseline, changed


def oracle_report(result):
    expected_rows = [row for row in result["operations"] if row.get("expected_cache") is not None]
    return {
        "expected_reusable": sum(row["expected_cache"] for row in expected_rows),
        "actual_replayed": sum(bool(row.get("cached")) for row in expected_rows),
        "missed_reuse": sum(row["expected_cache"] and not row.get("cached", False) for row in expected_rows),
        "unexpected_replay": sum(not row["expected_cache"] and row.get("cached", False) for row in expected_rows),
        "interpretation": "semantic oracle; safe rejection is missed reuse, not a false hit; inspect execution.cache rejection counters",
    }


def tracing_command(trace):
    agent = trace["agents"][0]
    index, operation = next((index, operation) for index, operation in enumerate(agent["operations"]) if operation["op"] == "bash")
    return {"agent": agent["id"], "index": index, "command": operation["command"],
            "sampling_rule": "first agent's first full test command, once per fresh repeated replay",
            "timing_scope": "Exec.Run duration, including on-demand materialize, scheduler queue, execution and dependency tracing; Absorb is timed in collect"}


def command_ns(result, selected):
    rows = [row for row in result["operations"] if row["agent"] == selected["agent"] and row["index"] == selected["index"]]
    if len(rows) != 1 or rows[0]["op"] != "bash":
        raise RuntimeError("registered tracing command is missing or ambiguous in replay report")
    return rows[0]["duration_ns"]


def benchmark_lines(results, language, selected):
    lines = []
    for result in results:
        lines.extend([f"BenchmarkReplay/{language}-1 1 {result['wall_ns']} ns/op",
                      f"BenchmarkCommand/{language}-1 1 {command_ns(result, selected)} ns/op"])
    return "\n".join(lines) + "\n"


def frozen_inputs(tmload, trace, protocol, pi_source):
    paths = {"tmload_sha256": Path(tmload), "trace_sha256": Path(trace),
             "registration_sha256": Path(protocol),
             "pi_build_manifest_sha256": Path(pi_source) / ".threadmill-benchmark-build.json"}
    return {field: hashlib.sha256(path.read_bytes()).hexdigest() for field, path in paths.items()}


def compare(args):
    root, output = Path(args.root).resolve(), Path(args.output).resolve()
    root.mkdir(parents=True, exist_ok=True)
    output.mkdir(parents=True, exist_ok=True)
    if (output / "registration.json").exists():
        raise RuntimeError("refusing to overwrite an existing registered run")
    benchstat = shutil.which("benchstat")
    if not args.smoke and not benchstat:
        raise RuntimeError("formal tracing comparison requires benchstat on PATH")
    if args.repeats < 1:
        raise RuntimeError("repeats must be positive")
    if not args.smoke:
        runtime.formal_preflight(args.tmload)
        if args.repeats != 10:
            raise RuntimeError("formal tracing comparison requires 10 repeats")
    if args.language == "pytest":
        runtime.run([args.python, "-m", "pytest", "--version"])
    repo, commit, baseline, changed = make_fixture(root, args.language, args.python, args.go)
    trace = script_trace(args.language, commit, args.agents, args.seed, args.edit_probability, baseline, changed, args.python, args.go)
    trace["fixture"]["files"] = len(runtime.run(["git", "-C", repo, "ls-files"]).splitlines())
    trace_path = output / "script.json"
    runtime.dump(trace_path, trace)
    selected_command = tracing_command(trace)
    protocol = Path(args.registration).resolve()
    manifest = {
        **frozen_inputs(args.tmload, trace_path, protocol, args.pi_source),
        "formal": not args.smoke, "threadmill_commit": runtime.run(["git", "-C", runtime.PROJECT, "rev-parse", "HEAD"]),
        "threadmill_dirty": bool(runtime.run(["git", "-C", runtime.PROJECT, "status", "--porcelain"])),
        "go_binary_metadata": runtime.run(["go", "version", "-m", args.tmload]),
        "pi_commit": runtime.PIN["commit"], "fixture_commit": commit,
        "pi_build": json.loads((Path(args.pi_source) / ".threadmill-benchmark-build.json").read_text()),
        "language": args.language, "agents": args.agents, "repeats": args.repeats, "seed": args.seed,
        "tracing_tax_command": selected_command,
        "edit_probability": args.edit_probability, "sandbox": args.sandbox,
        "uname": runtime.run(["uname", "-a"]), "cpu": runtime.run(["lscpu", "-J"]),
        "mount": runtime.run(["findmnt", "-J", "-T", root]), "argv": sys.argv,
        "go_version": runtime.run([args.go, "version"], env={**os.environ, "GOTOOLCHAIN": "local"}) if args.language == "go" else None,
        "python_version": runtime.run([args.python, "--version"]) if args.language == "pytest" else None,
        "python_packages": runtime.run([args.python, "-m", "pip", "freeze"]) if args.language == "pytest" else None,
        "strace_version": runtime.run([os.environ.get("THREADMILL_STRACE_BINARY", "strace"), "-V"]),
        "benchstat_sha256": hashlib.sha256(Path(benchstat).read_bytes()).hexdigest() if benchstat else None,
        "benchstat_binary": benchstat,
        "environment": {key: os.environ.get(key) for key in ["PATH", "LANG", "LC_ALL", "GOFLAGS", "PYTHONPATH"]},
        "harness_sha256": {str(path.relative_to(runtime.PROJECT)): hashlib.sha256(path.read_bytes()).hexdigest()
                            for path in [Path(__file__).resolve(), runtime.HERE / "bench.py", runtime.HERE / "replay.mjs"]},
    }
    runtime.dump(output / "registration.json", manifest)
    subprocess_environment = dict(os.environ)
    results = {}
    groups = ["cache-off-traced", "cache-on-traced", "pi-worktree", "cache-off-untraced"]
    for repeat in range(args.repeats):
        for group in groups[repeat % len(groups):] + groups[:repeat % len(groups)]:
            label = f"{group}-r{repeat + 1}"
            directory, report_path = root / label, output / f"{label}.json"
            if directory.exists() or report_path.exists():
                raise RuntimeError(f"refusing to overwrite {label}")
            directory.mkdir()
            group_environment = dict(subprocess_environment)
            temporary = directory / "tmp"
            temporary.mkdir()
            group_environment["TMPDIR"] = str(temporary)
            # Every repeated round begins cold; within it all Pi worktrees share GOCACHE.
            # TM owns its per-environment runtime HOME and does not forward GOCACHE.
            if args.language == "go" and group == "pi-worktree":
                group_environment["GOCACHE"] = str(directory / "shared-gocache")
                group_environment["GOTOOLCHAIN"] = "local"
            local_repo = directory / "repo"
            runtime.run(["cp", "-a", "--reflink=always", repo, local_repo])
            if group == "pi-worktree":
                argv = ["node", runtime.HERE / "replay.mjs", "--pi-source", args.pi_source,
                        "--repo", local_repo, "--workdir", directory / "worktrees", "--trace-in", trace_path, "--json-out", report_path]
            else:
                argv = [args.tmload, "-trace-in", trace_path, "-repo", local_repo, "-workdir", directory / "runtime",
                        "-sandbox", args.sandbox, "-slots", "1", "-cache=" + str(group == "cache-on-traced").lower(),
                        "-dependency-tracing=" + str(group != "cache-off-untraced").lower(),
                        "-cache-verify-sample-rate", "0", "-retain=true", "-json-out", report_path]
            with report_path.with_suffix(".log").open("w") as log:
                code = subprocess.run([str(value) for value in argv], stdout=log, stderr=subprocess.STDOUT, env=group_environment).returncode
            if not report_path.exists():
                raise RuntimeError(f"{label} failed before producing a report; see {report_path.with_suffix('.log')}")
            result = json.loads(report_path.read_text())
            result.update(group=group, repeat=repeat + 1, process_exit=code)
            runtime.check_terminal_state(result)
            if group == "cache-on-traced":
                result["oracle"] = oracle_report(result)
            results.setdefault(group, []).append(result)
            runtime.dump(report_path, result)
            print(json.dumps({"run": label, "wall_ns": result["wall_ns"], "errors": result["errors"]}), flush=True)
            shutil.rmtree(directory)
            if code or result["errors"] or result.get("oracle", {}).get("unexpected_replay"):
                raise RuntimeError(f"{label} correctness failed; retained report at {report_path}")
    off, on = output / "tracing-off.txt", output / "tracing-on.txt"
    off.write_text(benchmark_lines(results["cache-off-untraced"], args.language, selected_command))
    on.write_text(benchmark_lines(results["cache-off-traced"], args.language, selected_command))
    if benchstat:
        (output / "benchstat.txt").write_text(runtime.run([benchstat, off, on]) + "\n")
    medians = {group: statistics.median(result["wall_ns"] for result in rows) for group, rows in results.items()}
    cached = results["cache-on-traced"]
    saved = statistics.median(result["execution"]["cache"].get("saved_duration", 0) for result in cached)
    stores = statistics.median(result["execution"]["cache"].get("store_duration", 0) for result in cached)
    tax = medians["cache-off-traced"] - medians["cache-off-untraced"]
    command_medians = {group: statistics.median(command_ns(result, selected_command) for result in results[group])
                       for group in ["cache-off-traced", "cache-off-untraced"]}
    summary = {"language": args.language, "fixture_commit": commit, "threadmill_commit": runtime.run(["git", "-C", runtime.PROJECT, "rev-parse", "HEAD"]),
               "trace_sha256": hashlib.sha256(trace_path.read_bytes()).hexdigest(), "repeats": args.repeats, "wall_ns_median": medians,
               "tracing_tax_ns": tax, "store_duration_ns": stores, "historical_service_saved_ns": saved,
               "tracing_tax_command": selected_command, "command_duration_ns_median": command_medians,
               "command_tracing_tax_ns": command_medians["cache-off-traced"] - command_medians["cache-off-untraced"],
               "net_wall_saved_ns": medians["cache-off-untraced"] - medians["cache-on-traced"],
               "estimated_net_service_saved_ns": saved - tax - stores, "benchstat_available": bool(benchstat),
               "benchstat_command": ["benchstat", str(off), str(on)], "pi_shared_gocache": "one fresh per-repeat shared-gocache directory outside all worktrees",
               "warm_policy": "each repeated round starts cold; Pi shares native GOCACHE across agents within a round; TM current per-env HOME policy",
               "go_executable": args.go, "python_executable": args.python,
               "formal": not args.smoke}
    runtime.dump(output / "summary.json", summary)


def parser():
    cli = argparse.ArgumentParser(description=__doc__)
    for flag in ["root", "output", "tmload", "pi-source"]:
        cli.add_argument(f"--{flag}", required=True)
    cli.add_argument("--language", choices=["go", "pytest"], required=True)
    cli.add_argument("--python", default="python3")
    cli.add_argument("--go", default="go", help="exact toolchain binary; must be reachable in bwrap's /usr mount")
    cli.add_argument("--sandbox", choices=["external", "bwrap"], default="bwrap")
    cli.add_argument("--agents", type=int, default=16)
    cli.add_argument("--seed", type=int, default=42)
    cli.add_argument("--edit-probability", type=float, default=0.25)
    cli.add_argument("--repeats", type=int, default=10)
    cli.add_argument("--smoke", action="store_true", help="development validation; no formal performance claims")
    cli.add_argument("--registration", default=str(runtime.PROJECT / "docs/cache-worktree-benchmark-protocol.md"))
    return cli


if __name__ == "__main__":
    try:
        compare(parser().parse_args())
    except (RuntimeError, ValueError, OSError, subprocess.CalledProcessError) as error:
        print(f"cache-runtime: {error}", file=sys.stderr)
        sys.exit(1)
