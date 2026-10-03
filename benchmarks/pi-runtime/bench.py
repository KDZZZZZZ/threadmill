#!/usr/bin/env python3
"""Pinned, model-free native Pi / Threadmill replay and physical Btrfs measurements."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import statistics
import subprocess
import sys
import time

HERE = Path(__file__).resolve().parent
PROJECT = HERE.parents[1]
WIDTHS = [64, 128, 192, 256, 384, 448, 500, 576]
PIN = json.loads((HERE / "pi-pin.json").read_text())


def run(argv, cwd=None, env=None):
    completed = subprocess.run([str(arg) for arg in argv], cwd=cwd, env=env, check=True, capture_output=True, text=True)
    return completed.stdout.strip()


def dump(path, value):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, indent=2) + "\n")


def formal_preflight(tmload):
    commit = run(["git", "-C", PROJECT, "rev-parse", "HEAD"])
    if run(["git", "-C", PROJECT, "status", "--porcelain"]):
        raise RuntimeError("formal measurements require a clean committed Threadmill checkout; use --smoke during development")
    if subprocess.run(["git", "-C", PROJECT, "merge-base", "--is-ancestor", commit, "origin/main"], capture_output=True).returncode:
        raise RuntimeError("formal measurements require a Threadmill commit already integrated into origin/main through a PR")
    build = run(["go", "version", "-m", tmload])
    if f"vcs.revision={commit}" not in build or "vcs.modified=false" not in build:
        raise RuntimeError("tmload binary must be built from the exact clean registered Threadmill commit")
    return commit


def terminal_state_errors(row):
    if not row.get("backend", "").startswith("threadmill-"):
        return {}
    fields = {
        "execution": ["runtime_cleanup_errors", "runtime_dirs", "active", "queued", "heavy_active", "heavy_queued", "tracked_process_groups"],
        "vfs": ["materialize_active", "absorb_active", "overlay_active"],
    }
    return {f"{section}.{field}": row.get(section, {}).get(field)
            for section, names in fields.items() for field in names if row.get(section, {}).get(field) != 0}


def check_terminal_state(row):
    row["terminal_state_errors"] = terminal_state_errors(row)
    row["operation_errors"] = row["errors"]
    if row["terminal_state_errors"]:
        row["errors"] += 1


def summarize(rows, required_repeats=None, terminal_errors=terminal_state_errors):
    """The rule is frozen in docs/cache-worktree-benchmark-protocol.md before runs."""
    out = {}
    for backend in sorted({row["backend"] for row in rows}):
        selected = [row for row in rows if row["backend"] == backend]
        widths = sorted({row["agents"] for row in selected})
        tiers = []
        for width in widths:
            samples = [row for row in selected if row["agents"] == width]
            tier = {"agents": width, "repeats": len(samples),
                    "wall_ns_median": statistics.median(row["wall_ns"] for row in samples),
                    "errors": sum(max(row["errors"], bool(terminal_errors(row))) for row in samples)}
            for field, values in {
                "commands_per_second_median": [row.get("commands_per_second") for row in samples],
                "physical_peak_delta_bytes_median": [row.get("physical_disk", {}).get("peak_delta_bytes") for row in samples],
                "physical_retained_delta_bytes_median": [row.get("physical_disk", {}).get("retained_delta_bytes") for row in samples],
            }.items():
                if all(value is not None for value in values):
                    tier[field] = statistics.median(values)
            for operation in ["write", "fork", "collect"]:
                for percentile in ["p50_ns", "p95_ns"]:
                    values = [row.get("latency", {}).get(operation, {}).get(percentile) for row in samples]
                    if all(value is not None for value in values):
                        tier[f"{operation}_{percentile}_median"] = statistics.median(values)
            tiers.append(tier)
        limit = tiers[0]["wall_ns_median"] * 1.25
        eligible = [tier["agents"] for tier in tiers if tier["errors"] == 0
                    and tier["wall_ns_median"] <= limit
                    and (required_repeats is None or tier["repeats"] == required_repeats)]
        if tiers[0]["errors"] or (required_repeats is not None and tiers[0]["repeats"] != required_repeats):
            eligible = []
        peak = max(eligible) if eligible else None
        previous = widths[widths.index(peak) - 1] if peak is not None and widths.index(peak) > 0 else None
        out[backend] = {"effective_peak": peak, "stable_width": previous,
                        "wall_ns_limit": limit, "tiers": tiers}
    return out


def prepare_pi(args):
    source, cache = Path(args.source).resolve(), Path(args.cache).resolve()
    cache.mkdir(parents=True, exist_ok=True)
    if not source.exists():
        if args.offline:
            raise RuntimeError("offline prepare requires the pinned source checkout")
        source.parent.mkdir(parents=True, exist_ok=True)
        run(["git", "clone", "--filter=blob:none", "--no-checkout", PIN["repository"], source])
        run(["git", "-C", source, "checkout", "--detach", PIN["commit"]])
    if run(["git", "-C", source, "rev-parse", "HEAD"]) != PIN["commit"]:
        raise RuntimeError("Pi source is not at the pinned commit")
    log = cache / "prepare-pi.log"
    commands = [PIN["install"] + ["--cache", str(cache / "npm")]]
    if args.offline:
        commands[0].append("--offline")
    model_cache = cache / "model-data"
    model_dest = source / "packages/ai/src/providers/data"
    if model_cache.exists():
        shutil.copytree(model_cache, model_dest, dirs_exist_ok=True)
    with log.open("w") as stream:
        for argv in commands:
            subprocess.run(argv, cwd=source, check=True, stdout=stream, stderr=subprocess.STDOUT)
        probe = subprocess.run(["npm", "run", "check:model-data"], cwd=source, stdout=stream, stderr=subprocess.STDOUT)
        if probe.returncode:
            if args.offline:
                raise RuntimeError(f"offline model-data cache is missing or stale; see {log}")
            subprocess.run(["npm", "run", "hydrate:model-data"], cwd=source, check=True, stdout=stream, stderr=subprocess.STDOUT)
        if not args.offline:
            shutil.copytree(model_dest, model_cache, dirs_exist_ok=True)
        subprocess.run(PIN["build"], cwd=source, check=True, stdout=stream, stderr=subprocess.STDOUT)
    files = {str(path.relative_to(model_dest)): {"sha256": hashlib.sha256(path.read_bytes()).hexdigest(), "bytes": path.stat().st_size}
             for path in sorted(model_dest.rglob("*")) if path.is_file()}
    manifest = {"pi_commit": PIN["commit"], "node": run(["node", "--version"]), "npm": run(["npm", "--version"]),
                "package_lock_sha256": hashlib.sha256((source / "package-lock.json").read_bytes()).hexdigest(),
                "model_data": files, "offline_install": args.offline, "offline_build": True}
    manifest["tool_files"] = {spec.split(":")[0]: hashlib.sha256((source / spec.split(":")[0]).read_bytes()).hexdigest()
                              for spec in PIN["tools"].values()}
    dump(cache / "pi-build.json", manifest)
    dump(source / ".threadmill-benchmark-build.json", manifest)
    print(json.dumps({"pi_commit": PIN["commit"], "manifest": str(cache / "pi-build.json"),
                      "model_files": len(files), "offline_install": args.offline, "offline_build": True}))


def commit_fixture(repo):
    run(["git", "-C", repo, "init", "--quiet"])
    run(["git", "-C", repo, "config", "gc.auto", "0"])
    run(["git", "-C", repo, "add", "-A"])
    environment = {**os.environ, "GIT_AUTHOR_DATE": "2000-01-01T00:00:00Z", "GIT_COMMITTER_DATE": "2000-01-01T00:00:00Z"}
    run(["git", "-C", repo, "-c", "user.name=Benchmark", "-c", "user.email=benchmark@example.invalid",
         "commit", "--quiet", "-m", "Frozen benchmark fixture"], env=environment)
    return run(["git", "-C", repo, "rev-parse", "HEAD"])


def fixture(args):
    repo = Path(args.repo).resolve()
    if repo.exists():
        raise RuntimeError("fixture destination must not exist")
    repo.parent.mkdir(parents=True, exist_ok=True)
    if args.source_repo:
        if not args.commit:
            raise RuntimeError("real repository fixture requires a full --commit")
        run(["git", "clone", "--no-hardlinks", "--no-checkout", args.source_repo, repo])
        run(["git", "-C", repo, "checkout", "--detach", args.commit])
        run(["git", "-C", repo, "config", "gc.auto", "0"])
        commit = run(["git", "-C", repo, "rev-parse", "HEAD"])
        if commit != args.commit:
            raise RuntimeError("fixture commit must be a full resolved SHA-1")
    else:
        data = (b"0123456789" * ((args.file_bytes + 9) // 10))[:args.file_bytes]
        for index in range(args.files):
            target = repo / f"src/pkg-{index % 64}/file_{index}.txt"
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(data)
        commit = commit_fixture(repo)
    print(json.dumps({"repo": str(repo), "commit": commit, "files": len(run(["git", "-C", repo, "ls-files"]).splitlines())}))


def physical_sample(root, detailed=False):
    # df reports allocated physical filesystem space; apparent file size cannot.
    if detailed:
        run(["btrfs", "filesystem", "sync", root])
    usage = os.statvfs(root)
    sample = {"time_ns": time.time_ns(), "df_used_bytes": (usage.f_blocks - usage.f_bfree) * usage.f_frsize,
              "df_available_bytes": usage.f_bavail * usage.f_frsize}
    if detailed:
        sample["btrfs_du_raw"] = run(["btrfs", "filesystem", "du", "-s", "--raw", root])
    return sample


def measure(argv, root, output, sample_interval=0.2,
            terminal_check=check_terminal_state, measured_output=None):
    temporary = Path(root) / "tmp"
    temporary.mkdir()
    before = physical_sample(root, detailed=True)
    samples = [before]
    with Path(output).with_suffix(".log").open("w") as log:
        process = subprocess.Popen([str(arg) for arg in argv], stdout=log, stderr=subprocess.STDOUT,
                                   env={**os.environ, "TMPDIR": str(temporary)})
        try:
            while process.poll() is None:
                samples.append(physical_sample(root))
                time.sleep(sample_interval)
        finally:
            if process.poll() is None:
                process.kill()
            code = process.wait()
    after = physical_sample(root, detailed=True)
    samples.append(after)
    peak = max(samples, key=lambda sample: sample["df_used_bytes"])
    disk = {"before": before, "peak": peak, "after": after,
            "peak_delta_bytes": peak["df_used_bytes"] - before["df_used_bytes"],
            "retained_delta_bytes": after["df_used_bytes"] - before["df_used_bytes"],
            "sample_interval_seconds": sample_interval, "samples": samples,
            "scope": "entire dedicated filesystem; df includes metadata; sampled peak"}
    if Path(output).exists():
        result = json.loads(Path(output).read_text())
    else:
        result = {"errors": 1, "wall_ns": 0, "operations": [], "latency": {}}
    result["physical_disk"], result["process_exit"] = disk, code
    terminal_check(result)
    if code and not result.get("errors"):
        result["errors"] = 1
    dump(measured_output or output, result)
    return result


def doctor(args):
    root = Path(args.root).resolve()
    root.mkdir(parents=True, exist_ok=True)
    if run(["stat", "-f", "-c", "%T", root]) != "btrfs":
        raise RuntimeError("benchmark root must be Btrfs")
    probe = root / ".doctor-reflink"
    probe.mkdir(exist_ok=False)
    try:
        (probe / "source").write_bytes(b"reflink probe" * 4096)
        run(["cp", "--reflink=always", probe / "source", probe / "clone"])
        if (probe / "clone").read_bytes() != (probe / "source").read_bytes():
            raise RuntimeError("reflink probe data mismatch")
    finally:
        shutil.rmtree(probe)
    if args.bwrap:
        run(["bwrap", "--ro-bind", "/", "/", "--proc", "/proc", "--dev", "/dev", "--", "true"])
    state = {"btrfs": True, "reflink": True, "bwrap": args.bwrap, "mount": run(["findmnt", "-J", "-T", root]),
             "physical": physical_sample(root, True)}
    print(json.dumps(state, indent=2))


def matrix(args):
    widths = [int(width) for width in args.widths.split(",")]
    if args.repeats < 1 or any(width <= 0 for width in widths) or len(set(widths)) != len(widths):
        raise RuntimeError("widths and repeats must be positive, with no duplicate widths")
    if not args.smoke and (widths != WIDTHS or args.repeats != 3):
        raise RuntimeError("formal matrix requires widths 64,128,192,256,384,448,500,576 and 3 repeats; use --smoke for smaller runs")
    root, output = Path(args.root).resolve(), Path(args.output).resolve()
    output.mkdir(parents=True, exist_ok=True)
    if (output / "registration.json").exists():
        raise RuntimeError("refusing to overwrite an existing registered run")
    root.mkdir(parents=True, exist_ok=True)
    if run(["stat", "-f", "-c", "%T", root]) != "btrfs":
        raise RuntimeError("matrix requires Btrfs")
    if not args.smoke and not args.dedicated_volume:
        raise RuntimeError("formal physical measurements require --dedicated-volume after provisioning a separate filesystem")
    registration = Path(args.registration).resolve()
    if not registration.exists():
        raise RuntimeError("preregistration document must exist before running")
    commit = run(["git", "-C", PROJECT, "rev-parse", "HEAD"])
    dirty = run(["git", "-C", PROJECT, "status", "--porcelain"])
    if not args.smoke:
        formal_preflight(args.tmload)
    groups = args.groups.split(",")
    if set(groups) - {"threadmill-bwrap", "threadmill-external", "pi-worktree", "pi-shared-cwd"}:
        raise RuntimeError("unknown group")
    metadata = {"formal": not args.smoke, "threadmill_commit": commit, "dirty": bool(dirty), "pi_commit": PIN["commit"],
                "fixture_commit": run(["git", "-C", args.fixture, "rev-parse", "HEAD"]),
                "registration_sha256": hashlib.sha256(registration.read_bytes()).hexdigest(), "widths": widths,
                "repeats": args.repeats, "groups": groups, "uname": run(["uname", "-a"]),
                "cpu": run(["lscpu", "-J"]), "btrfs_mount": run(["findmnt", "-J", "-T", root]),
                "tmload_sha256": hashlib.sha256(Path(args.tmload).read_bytes()).hexdigest(),
                "go_binary_metadata": run(["go", "version", "-m", args.tmload]), "argv": sys.argv}
    pi_manifest = Path(args.pi_source) / ".threadmill-benchmark-build.json"
    metadata["pi_build"] = json.loads(pi_manifest.read_text())
    metadata["pi_build_manifest_sha256"] = hashlib.sha256(pi_manifest.read_bytes()).hexdigest()
    metadata["harness_sha256"] = {str(path.relative_to(PROJECT)): hashlib.sha256(path.read_bytes()).hexdigest()
                                  for path in [Path(__file__).resolve(), HERE / "replay.mjs"]}
    metadata["traces"] = {}
    for width in widths:
        trace = output / f"trace-{width}.json"
        if trace.exists():
            raise RuntimeError(f"refusing to overwrite {trace}")
        run([args.tmload, "-trace-out", trace, "-repo", args.fixture, "-agents", width,
             "-turns", args.turns, "-seed", args.seed, "-time-scale", args.time_scale, "-command-duty", args.command_duty])
        metadata["traces"][str(width)] = hashlib.sha256(trace.read_bytes()).hexdigest()
    dump(output / "registration.json", metadata)
    rows = []
    for width in widths:
        trace = output / f"trace-{width}.json"
        for repeat in range(args.repeats):
            # Rotate order within repeats; runs themselves are strictly serial.
            for group in groups[repeat % len(groups):] + groups[:repeat % len(groups)]:
                label = f"{group}-w{width}-r{repeat + 1}"
                runroot, result_path = root / label, output / f"{label}.json"
                if runroot.exists() or result_path.exists():
                    raise RuntimeError(f"refusing to overwrite {label}")
                runroot.mkdir()
                repo = runroot / "repo"
                run(["cp", "-a", "--reflink=always", Path(args.fixture).resolve(), repo])
                if group.startswith("threadmill-"):
                    argv = [args.tmload, "-trace-in", trace, "-repo", repo, "-workdir", runroot / "runtime",
                            "-sandbox", group.removeprefix("threadmill-"), "-slots", args.slots,
                            "-retain=true", "-cache=false", "-dependency-tracing=false", "-json-out", result_path]
                else:
                    argv = ["node", HERE / "replay.mjs", "--pi-source", args.pi_source, "--repo", repo,
                            "--workdir", runroot / "worktrees", "--trace-in", trace, "--json-out", result_path]
                    if group == "pi-shared-cwd":
                        argv.append("--shared-cwd")
                result = measure(argv, runroot, result_path)
                result.update(backend=group, agents=width, repeat=repeat + 1, formal=not args.smoke)
                rows.append(result)
                dump(result_path, result)
                print(json.dumps({"run": label, "wall_ns": result["wall_ns"], "errors": result["errors"]}), flush=True)
                shutil.rmtree(runroot)
        dump(output / "summary.json", {"metadata": metadata, "capacity": summarize(rows, args.repeats)})


def parser():
    cli = argparse.ArgumentParser(description=__doc__)
    sub = cli.add_subparsers(dest="command", required=True)
    pi = sub.add_parser("prepare-pi")
    pi.add_argument("--source", required=True)
    pi.add_argument("--cache", required=True)
    pi.add_argument("--offline", action="store_true")
    pi.set_defaults(func=prepare_pi)
    create = sub.add_parser("fixture")
    create.add_argument("--repo", required=True)
    create.add_argument("--files", type=int, default=3000)
    create.add_argument("--file-bytes", type=int, default=4096)
    create.add_argument("--source-repo")
    create.add_argument("--commit")
    create.set_defaults(func=fixture)
    check = sub.add_parser("doctor")
    check.add_argument("--root", required=True)
    check.add_argument("--bwrap", action="store_true")
    check.set_defaults(func=doctor)
    compare = sub.add_parser("matrix")
    for option in ["root", "output", "fixture", "tmload", "pi-source"]:
        compare.add_argument(f"--{option}", required=True)
    compare.add_argument("--registration", default=str(PROJECT / "docs/cache-worktree-benchmark-protocol.md"))
    compare.add_argument("--dedicated-volume", action="store_true")
    compare.add_argument("--smoke", action="store_true")
    compare.add_argument("--widths", default=",".join(map(str, WIDTHS)))
    compare.add_argument("--repeats", type=int, default=3)
    compare.add_argument("--turns", type=int, default=40)
    compare.add_argument("--seed", type=int, default=42)
    compare.add_argument("--slots", type=int, default=8)
    compare.add_argument("--time-scale", type=float, default=0.05)
    compare.add_argument("--command-duty", type=float, default=0.12)
    compare.add_argument("--groups", default="threadmill-bwrap,threadmill-external,pi-worktree")
    compare.set_defaults(func=matrix)
    return cli


if __name__ == "__main__":
    try:
        arguments = parser().parse_args()
        arguments.func(arguments)
    except (RuntimeError, ValueError, OSError, subprocess.CalledProcessError) as error:
        print(f"pi-runtime: {error}", file=sys.stderr)
        sys.exit(1)
