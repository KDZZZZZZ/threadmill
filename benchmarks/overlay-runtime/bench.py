#!/usr/bin/env python3
"""98ed7d3 native Overlay compatibility benchmark."""

import argparse
import hashlib
import importlib.util
import json
import math
import os
from pathlib import Path
import shutil
import subprocess
import sys

from launch import lower_backing

HERE = Path(__file__).resolve().parent
BASE = "98ed7d3d38c0fc956de0bc70c858f4ef65c90e02"
BACKEND = "threadmill-historical98-overlay-external"
OBJECTS = {"internal": "7c119197cba0de95a940f6742f654b49b40d43ff",
           "go.mod": "f1c18d82019e44e7e1325b04d5ee995214f9262b", "go.sum": "d30ffee860e43ab46315b34632e3ab3b97509b27"}


def sha(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def emit(path, value):
    with Path(path).open("x") as stream:
        json.dump(value, stream, indent=2)
        stream.write("\n")


def shared(project):
    spec = importlib.util.spec_from_file_location("pi_runtime_bench", Path(project) / "benchmarks/pi-runtime/bench.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def git(runtime, repo, *args):
    return runtime.run(["git", "-c", "safe.directory=" + str(Path(repo).resolve()), "-C", repo, *args],
                       env={**os.environ, "GIT_OPTIONAL_LOCKS": "0"})


def prepare(args):
    if HERE != (Path(args.project).resolve() / "benchmarks/overlay-runtime"):
        raise RuntimeError("run the reviewed runner/templates from the registered project checkout")
    runtime = shared(args.project)
    source_commit = git(runtime, args.project, "rev-parse", "HEAD")
    if git(runtime, args.project, "status", "--porcelain"):
        raise RuntimeError("template/main source must be a clean committed PR result")
    git(runtime, args.project, "merge-base", "--is-ancestor", source_commit, "origin/main")
    destination = Path(args.clone).resolve()
    binary = Path(args.binary).resolve()
    if destination.exists() or binary.exists() or Path(args.provenance).exists():
        raise RuntimeError("refusing to overwrite a clone, binary or provenance")
    build_environment = {**os.environ, "GOTOOLCHAIN": "local", "GOWORK": "off", "GOFLAGS": ""}
    if "go1.24.2 " not in runtime.run([args.go, "version"], env=build_environment):
        raise RuntimeError("requires installed Go 1.24.2")
    runtime.run(["git", "clone", "--no-hardlinks", "--no-checkout", Path(args.project).resolve(), destination])
    git(runtime, destination, "checkout", "--detach", BASE)
    template_hashes = {}
    adapter = destination / "cmd/tmlegacy"
    adapter.mkdir()
    for name in ["main", "trace", "audit"]:
        template = HERE / "adapter" / (name + ".go.txt")
        template_hashes[template.name] = sha(template)
        shutil.copyfile(template, adapter / (name + ".go"))
    git(runtime, destination, "add", "cmd/tmlegacy")
    git(runtime, destination, "-c", "user.name=Benchmark", "-c", "user.email=benchmark@example.invalid",
        "commit", "--quiet", "-m", "Benchmark adapter from " + source_commit)
    bridge = git(runtime, destination, "rev-parse", "HEAD")
    actual_objects = {name: git(runtime, destination, "rev-parse", "HEAD:" + name) for name in OBJECTS}
    if actual_objects != OBJECTS or git(runtime, destination, "status", "--porcelain"):
        raise RuntimeError("old internal/module objects changed or bridge checkout is dirty")
    if git(runtime, destination, "diff", "--name-only", BASE, "HEAD").splitlines() != [
            "cmd/tmlegacy/audit.go", "cmd/tmlegacy/main.go", "cmd/tmlegacy/trace.go"]:
        raise RuntimeError("bridge must add only the reviewed benchmark command")
    binary.parent.mkdir(parents=True, exist_ok=True)
    runtime.run([args.go, "build", "-mod=readonly", "-buildvcs=true", "-o", binary, "./cmd/tmlegacy"],
                cwd=destination, env=build_environment)
    metadata = runtime.run([args.go, "version", "-m", binary], env=build_environment)
    if f"vcs.revision={bridge}" not in metadata or "vcs.modified=false" not in metadata:
        raise RuntimeError("ordinary-clone bridge VCS stamping failed")
    emit(args.provenance, {"version": 1, "runtime_base": BASE, "template_main_commit": source_commit,
                          "bridge_commit": bridge, "clone": str(destination), "runtime_objects": actual_objects,
                          "templates": template_hashes, "binary_sha256": sha(binary), "go_build_metadata": metadata,
                          "harness_sha256": {name: sha(HERE / name) for name in ["bench.py", "launch.py"]}})


def terminal_errors(row):
    fields = {"execution": ["queued", "active", "heavy_queued", "heavy_active", "tracked_process_groups", "runtime_dirs",
                            "adapter_reap_errors", "adapter_orphan_runtime_dirs", "adapter_runtime_dir_scan_errors"],
              "vfs": ["materialize_active", "absorb_active", "overlay_active"]}
    errors = {f"{section}.{name}": row.get(section, {}).get(name)
              for section, names in fields.items() for name in names
              if type(row.get(section, {}).get(name)) is not int or row[section][name] != 0}
    if type(row.get("checkpoint_close_proof_errors")) is not int or row["checkpoint_close_proof_errors"] != 0:
        errors["checkpoint_close_proof_errors"] = row.get("checkpoint_close_proof_errors")
    if type(row.get("final_close_errors")) is not int or row["final_close_errors"] != 0:
        errors["final_close_errors"] = row.get("final_close_errors")
    if type(row.get("final_close_ns")) is not int or row["final_close_ns"] < 0:
        errors["final_close_ns"] = row.get("final_close_ns")
    return errors


def terminal_check(row):
    row["terminal_state_errors"] = terminal_errors(row)
    row["operation_errors"] = row.get("errors", 1)
    row["errors"] = row.get("errors", 1) + bool(row["terminal_state_errors"])


def launch_errors(evidence, expected_exit):
    errors = []
    ns = evidence.get("namespace", {})
    for field, wanted in {"version": 1, "errors": 0, "signal": 0, "known_children_gone": True,
                          "parent_overlay_mounts_after": [], "process_exit": expected_exit}.items():
        if field not in evidence or evidence[field] != wanted:
            errors.append("launcher." + field)
    for field, wanted in {"euid": 0, "pid": 1, "readonly_sources": True, "overlay_mounts_after": [],
                          "children_after": [], "binary_exit": expected_exit}.items():
        if field not in ns or ns[field] != wanted:
            errors.append("namespace." + field)
    if not ns.get("mount_namespace") or not ns.get("parent_mount_namespace") or ns["mount_namespace"] == ns["parent_mount_namespace"]:
        errors.append("namespace.private_mount")
    return errors


def verify(args):
    method = []
    loaded = {}
    for name in ["trace", "replay", "measured", "replay_launch"]:
        try:
            loaded[name] = json.loads(Path(getattr(args, name)).read_text())
        except (OSError, ValueError) as error:
            loaded[name] = {}
            method.append(name + ": " + str(error))
    trace, replay, measured = loaded["trace"], loaded["replay"], loaded["measured"]
    out = dict(measured)
    operation_errors = replay.get("errors", 1)
    if type(operation_errors) is not int or operation_errors < 0:
        method.append("invalid operation error count")
        operation_errors = 1
    if operation_errors == 0 and replay.get("error"):
        method.append("unreported replay error")
    fixture = trace.get("fixture", {}).get("commit")
    trace_hash = sha(args.trace) if Path(args.trace).exists() else None
    agents = trace.get("agents", [])
    for name, wanted in {"version": 1, "backend": BACKEND, "runtime_commit": BASE, "fixture_commit": fixture,
                         "trace_sha256": trace_hash, "agents": len(agents)}.items():
        if name not in replay or replay[name] != wanted:
            method.append("replay identity " + name)
    method += list(terminal_errors(replay))
    disk = measured.get("physical_disk", {})
    for phase in ["before", "after"]:
        sample = disk.get(phase, {})
        if type(sample.get("df_used_bytes")) is not int or sample["df_used_bytes"] < 0 or not sample.get("btrfs_du_raw"):
            method.append("physical " + phase + " proof missing")
    if disk.get("sample_interval_seconds") != 0.2 or type(disk.get("peak", {}).get("df_used_bytes")) is not int:
        method.append("physical sampled peak proof missing")
    values = [disk.get(phase, {}).get("df_used_bytes") for phase in ["before", "peak", "after"]]
    if all(type(value) is int and value >= 0 for value in values):
        before, peak, after = values
        if (peak < max(before, after) or type(disk.get("peak_delta_bytes")) is not int or
                disk["peak_delta_bytes"] != peak - before or type(disk.get("retained_delta_bytes")) is not int or
                disk["retained_delta_bytes"] != after - before):
            method.append("physical allocation arithmetic differs")
    else:
        method.append("invalid physical allocation values")
    samples = disk.get("samples", [])
    sample_values = [sample.get("df_used_bytes") for sample in samples]
    if not sample_values or any(type(value) is not int or value < 0 for value in sample_values):
        method.append("physical sample evidence missing or invalid")
    elif disk.get("before") != samples[0] or disk.get("after") != samples[-1] or values[1] != max(sample_values):
        method.append("physical sampled peak differs from saved samples")
    for name in ["version", "backend", "runtime_commit", "fixture_commit", "trace_sha256", "agents", "wall_ns",
                 "errors", "serial", "setup_ns", "latency", "commands_per_second", "checkpoint_audit",
                 "operations", "execution", "vfs", "checkpoint_ids", "checkpoint_overlay_proofs", "checkpoint_close_proof_errors",
                 "final_close_ns", "final_close_errors"]:
        if name not in replay:
            method.append("missing raw field " + name)
    for name in replay.keys() | {"error"}:
        if (name in replay) != (name in measured) or type(measured.get(name)) is not type(replay.get(name)) or measured.get(name) != replay.get(name):
            method.append("measured/raw mismatch " + name)
    if replay.get("serial") is not trace.get("serial", False):
        method.append("replay serial differs from trace")
    if type(replay.get("wall_ns")) is not int or replay["wall_ns"] <= 0:
        method.append("missing wall timing")
    if type(replay.get("setup_ns")) is not int or replay["setup_ns"] < 0:
        method.append("invalid setup timing")
    throughput = replay.get("commands_per_second")
    if type(throughput) not in (int, float) or not math.isfinite(throughput) or throughput < 0:
        method.append("invalid command throughput")
    vfs = replay.get("vfs", {})
    if vfs.get("overlay_available") is not True or vfs.get("overlay_backend") != "native-overlayfs":
        method.append("native overlay selection")
    for name in ["overlay_error_fallbacks", "materialize_full_copies", "materialize_reflinks"]:
        if vfs.get(name) != 0:
            method.append("non-native materialization: " + name)
    code = measured.get("process_exit")
    method += launch_errors(loaded["replay_launch"], code)
    if type(code) is not int or (code != 0 and operation_errors == 0):
        method.append("unexplained replay process failure")
    operations = {(item.get("agent"), item.get("index")): item for item in replay.get("operations", [])}
    if len(operations) != len(replay.get("operations", [])):
        method.append("duplicate operation evidence")
    if operation_errors == 0 and any(item.get("error") for item in operations.values()):
        method.append("unreported operation error")
    expected_keys = set()
    op_counts = {}
    for (agent, index), item in operations.items():
        if type(agent) is not str or type(index) is not int:
            method.append("invalid operation key")
        duration = item.get("duration_ns")
        if type(duration) is not int or duration < 0:
            method.append(f"invalid operation duration {agent}:{index}")
        op = item.get("op")
        if type(op) is str:
            op_counts[op] = op_counts.get(op, 0) + 1
    latency = replay.get("latency")
    if type(latency) is not dict:
        method.append("invalid latency evidence")
    else:
        if set(latency) != set(op_counts):
            method.append("latency operation classes differ")
        for op, count in op_counts.items():
            sample = latency.get(op)
            if (type(sample) is not dict or type(sample.get("count")) is not int or sample["count"] != count or
                    any(type(sample.get(name)) is not int or sample[name] < 0 for name in ["p50_ns", "p95_ns"])):
                method.append("invalid latency " + op)
    for agent in agents:
        expected = [(-1, {"op": "fork"}), *enumerate(agent.get("operations", [])),
                    (len(agent.get("operations", [])), {"op": "collect"}),
                    (len(agent.get("operations", [])) + 1, {"op": "release"})]
        fork_failed = bool(operations.get((agent["id"], -1), {}).get("error"))
        for index, op in expected:
            if fork_failed and index >= 0 and op["op"] != "release":
                continue
            expected_keys.add((agent["id"], index))
            actual = operations.get((agent["id"], index))
            if actual is None or actual.get("op") != op["op"]:
                method.append(f"missing operation {agent['id']}:{index}")
            elif op["op"] == "bash" and actual.get("exit_code") != op.get("expected_exit", 0) and not actual.get("error"):
                method.append(f"unreported exit mismatch {agent['id']}:{index}")
    if set(operations) != expected_keys:
        method.append("operation key set differs from trace")
    out.update(backend=BACKEND, agents=len(agents), operation_errors=operation_errors)
    if operation_errors:
        out["checkpoint_audit"] = "not_applicable_failed_operation"
    else:
        expected_ids = sorted("checkpoint-" + agent["id"] for agent in agents)
        if sorted(replay.get("checkpoint_ids", [])) != expected_ids or replay.get("checkpoint_overlay_proofs") != len(agents):
            method.append("incomplete retained native checkpoint proof")
        try:
            audit = json.loads(Path(args.audit).read_text())
            audit_launch = json.loads(Path(args.audit_launch).read_text())
        except (OSError, ValueError) as error:
            audit, audit_launch = {}, {}
            method.append("audit missing or invalid: " + str(error))
        for name, wanted in {"version": 1, "runtime_commit": BASE, "fixture_commit": fixture, "trace_sha256": trace_hash,
                             "replay_report_sha256": sha(args.replay), "passed": True, "errors": 0,
                             "checkpoint_count": len(agents), "checkpoint_overlay_proofs": len(agents)}.items():
            if name not in audit or audit[name] != wanted:
                method.append("audit " + name)
        for name in ["materialize_active", "absorb_active", "overlay_active"]:
            if audit.get("vfs", {}).get(name) != 0:
                method.append("audit terminal " + name)
        method += launch_errors(audit_launch, 0)
        if not isinstance(audit.get("lower_tree_sha256"), str) or len(audit["lower_tree_sha256"]) != 64:
            method.append("audit lower content proof missing")
        if type(audit.get("audit_ns")) is not int or audit["audit_ns"] < 0:
            method.append("audit timing missing")
        out["checkpoint_audit"] = "failed" if method else "passed"
        out["audit_ns"] = audit.get("audit_ns")
        out["lower_tree_sha256"] = audit.get("lower_tree_sha256")
    out["method_errors"] = method
    out["failure_kind"] = "method" if method else ("capacity" if operation_errors else "none")
    out["errors"] = operation_errors + bool(method)
    out["evidence_sha256"] = {name: sha(getattr(args, name)) for name in
                              ["trace", "replay", "measured", "audit", "replay_launch", "audit_launch"]
                              if Path(getattr(args, name)).exists()}
    emit(args.json_out, out)
    return 1 if out["errors"] else 0


def matrix(args):
    if os.geteuid() != 0 or not args.dedicated_volume or not args.dedicated_lower_loop:
        raise RuntimeError("fixed legacy matrix requires the authorized root runner and dedicated upper volume")
    if HERE != (Path(args.project).resolve() / "benchmarks/overlay-runtime"):
        raise RuntimeError("run the reviewed runner/templates from the registered project checkout")
    runtime = shared(args.project)
    registered = json.loads(Path(args.main_registration).read_text())
    provenance = json.loads(Path(args.provenance).read_text())
    if registered["widths"] != runtime.WIDTHS or registered["repeats"] != 3:
        raise RuntimeError("reuse the exact eight-tier, three-repeat main registration")
    if provenance["runtime_base"] != BASE or provenance["runtime_objects"] != OBJECTS or sha(args.binary) != provenance["binary_sha256"]:
        raise RuntimeError("legacy runtime provenance differs")
    if git(runtime, args.project, "rev-parse", "HEAD") != provenance["template_main_commit"] or git(runtime, args.project, "status", "--porcelain"):
        raise RuntimeError("runner/templates must stay at the clean registered PR commit")
    git(runtime, args.project, "merge-base", "--is-ancestor", provenance["template_main_commit"], "origin/main")
    if git(runtime, args.fixture, "rev-parse", "HEAD") != registered["fixture_commit"]:
        raise RuntimeError("fixture differs from main registration")
    slots = runtime.parser().parse_args(registered["argv"][1:]).slots
    root, output = Path(args.root).resolve(), Path(args.output).resolve()
    output.mkdir(parents=True, exist_ok=True)
    if (output / "registration.json").exists():
        raise RuntimeError("refusing to overwrite a historical registration")
    backing = lower_backing(args.lower, root)
    lower_mount = runtime.run(["findmnt", "-J", "-T", args.lower])
    if json.loads(lower_mount)["filesystems"][0]["fstype"] != "ext4":
        raise RuntimeError("dedicated readonly lower loop filesystem must be ext4")
    traces = {}
    for width in runtime.WIDTHS:
        trace = Path(args.trace_dir) / f"trace-{width}.json"
        if sha(trace) != registered["traces"][str(width)]:
            raise RuntimeError("frozen main trace differs")
        traces[width] = trace.resolve()
    lower_before = runtime.physical_sample(args.lower)
    lower_before["scope"] = "this group's dedicated readonly ext4 loop filesystem bitmap usage; separate from upper physical delta"
    lower_before["fixture_du_raw"] = runtime.run(["du", "--summarize", "--block-size=1", "--", args.lower])
    lower_before["fixture_du_scope"] = "fixture subtree allocated file/directory blocks; excludes filesystem metadata"
    metadata = {"version": 1, "backend": BACKEND, "main_registration_sha256": sha(args.main_registration),
                "main_matrix_commit": registered["threadmill_commit"], "provenance": provenance, "lower_backing": backing,
                "provenance_sha256": sha(args.provenance), "lower_before": lower_before,
                "lower_mount": lower_mount,
                "upper_mount": runtime.run(["findmnt", "-J", "-T", root]), "euid": os.geteuid(),
                "kernel": runtime.run(["uname", "-a"]),
                "namespace_tools": {name: runtime.run(["/usr/bin/" + name, "--version"]) for name in ["unshare", "setpriv"]},
                "widths": runtime.WIDTHS, "repeats": 3, "expected_rows": 24, "slots": slots, "argv": sys.argv,
                "traces": registered["traces"], "fixture_commit": registered["fixture_commit"],
                "harness_sha256": {"shared": sha(Path(args.project) / "benchmarks/pi-runtime/bench.py"),
                                   "legacy": sha(__file__), "launcher": sha(HERE / "launch.py")},
                "layout": "historical dedicated readonly ext4 loop lower + dedicated Btrfs upper; no main-table capacity multiplier"}
    emit(output / "registration.json", metadata)
    rows = []
    for width in runtime.WIDTHS:
        for repeat in range(1, 4):
            label = f"{BACKEND}-w{width}-r{repeat}"
            runroot = root / label
            if runroot.exists():
                raise RuntimeError("refusing to overwrite " + label)
            runroot.mkdir()
            raw, measured, audit, final = [output / (label + suffix + ".json") for suffix in [".replay", ".measured", ".audit", ""]]
            replay_launch, audit_launch = [output / (label + suffix + ".json") for suffix in [".replay-launch", ".audit-launch"]]
            common = ["/usr/bin/setpriv", "--pdeathsig", "KILL", sys.executable, HERE / "launch.py",
                      "--binary", Path(args.binary).resolve(), "--binary-sha256", provenance["binary_sha256"],
                      "--trace", traces[width], "--trace-sha256", registered["traces"][str(width)],
                      "--repo", Path(args.fixture).resolve(), "--lower", Path(args.lower).resolve(),
                      "--live-root", runroot / "runtime", "--slots", str(slots)]
            try:
                result = runtime.measure([*common, "--json-out", raw, "--evidence-out", replay_launch], runroot, raw,
                                         terminal_check=terminal_check, measured_output=measured)
                if result.get("operation_errors") == 0:
                    with (output / (label + ".audit.log")).open("x") as log:
                        subprocess.run([str(arg) for arg in [*common, "--audit-only", "--replay-report", raw,
                                       "--json-out", audit, "--evidence-out", audit_launch]],
                                       env={**os.environ, "TMPDIR": str(runroot / "tmp")}, stdout=log, stderr=subprocess.STDOUT)
                verify(argparse.Namespace(trace=traces[width], replay=raw, measured=measured, audit=audit,
                                         replay_launch=replay_launch, audit_launch=audit_launch, json_out=final))
                row = json.loads(final.read_text())
            except BaseException as error:
                row = {"backend": BACKEND, "agents": width, "wall_ns": 0, "errors": 1,
                       "method_errors": [str(error)], "failure_kind": "method"}
                if not final.exists():
                    emit(final, row)
            row.update(repeat=repeat, formal=True)
            runtime.dump(final, row)
            rows.append(row)
            runtime.dump(output / "summary.json", {"metadata": metadata, "complete": False,
                         "capacity": runtime.summarize(rows, 3, terminal_errors=terminal_errors)})
            if row["method_errors"]:
                raise RuntimeError("method failure; raw evidence retained; stopping new rows: " + label)
            # Retained upper was sampled and audited before removal. Capacity
            # failures are retained in raw JSON and the matrix continues.
            shutil.rmtree(runroot)
    after = runtime.physical_sample(args.lower)
    after["fixture_du_raw"] = runtime.run(["du", "--summarize", "--block-size=1", "--", args.lower])
    runtime.dump(output / "lower-after.json", after)
    if after["df_used_bytes"] != lower_before["df_used_bytes"] or after["fixture_du_raw"] != lower_before["fixture_du_raw"] or lower_backing(args.lower, root) != backing:
        for row in rows:
            row["method_errors"].append("fixed lower allocation changed")
            row["errors"] += 1
            row["failure_kind"] = "method"
            runtime.dump(output / f"{BACKEND}-w{row['agents']}-r{row['repeat']}.json", row)
        runtime.dump(output / "summary.json", {"metadata": metadata, "lower_after": after, "complete": False,
                     "capacity": runtime.summarize(rows, 3, terminal_errors=terminal_errors)})
        raise RuntimeError("fixed lower allocation changed")
    runtime.dump(output / "summary.json", {"metadata": metadata, "lower_after": after, "complete": len(rows) == 24,
                 "capacity": runtime.summarize(rows, 3, terminal_errors=terminal_errors)})


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    subs = parser.add_subparsers(dest="command", required=True)
    build = subs.add_parser("prepare")
    for name in ["project", "clone", "binary", "provenance"]:
        build.add_argument("--" + name, required=True)
    build.add_argument("--go", default="go")
    build.set_defaults(action=prepare)
    check = subs.add_parser("verify-row")
    for name in ["trace", "replay", "measured", "audit", "replay-launch", "audit-launch", "json-out"]:
        check.add_argument("--" + name, type=Path, required=True)
    check.set_defaults(action=verify)
    run = subs.add_parser("matrix")
    for name in ["project", "main-registration", "trace-dir", "fixture", "lower", "root", "output", "binary", "provenance"]:
        run.add_argument("--" + name, required=True)
    run.add_argument("--dedicated-volume", action="store_true")
    run.add_argument("--dedicated-lower-loop", action="store_true", required=True,
                     help="this group requires a dedicated readonly ext4 loop filesystem, never a shared host bind")
    run.set_defaults(action=matrix)
    args = parser.parse_args()
    try:
        return args.action(args) or 0
    except Exception as error:
        if args.command == "verify-row" and not args.json_out.exists():
            emit(args.json_out, {"errors": 1, "failure_kind": "method", "method_errors": [str(error)]})
        raise


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as error:
        print(error, file=sys.stderr)
        sys.exit(1)
