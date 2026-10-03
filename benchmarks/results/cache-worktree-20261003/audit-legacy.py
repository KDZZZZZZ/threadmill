#!/usr/bin/env python3
"""Offline audit of the registered 48-row historical Overlay matrix.

Provide the unpacked legacy root (--raw), original primary root
(--primary-raw), frozen e177fa0 project checkout (--project), and a NEW output
directory. Original absolute paths are compared as recorded strings only.
No binary, loop image, mount, subprocess or model is used. The SHA-checked
verify-row helper runs unchanged except its final emit is captured in memory.
Saved checkpoint/namespace proofs are verified; checkpoints are not restored
again. Capacity failures remain in the summaries. Method failures abort.
"""

import argparse
from collections import Counter
import hashlib
import importlib.util
import json
from pathlib import Path, PurePosixPath
import sys

sys.dont_write_bytecode = True
WIDTHS = [64, 128, 192, 256, 384, 448, 500, 576]
FIXTURES = ["synthetic", "ipython"]
BACKEND = "threadmill-historical98-overlay-external"
PREREGISTRATION_SHA = "534aabba4ccb58eda15cb0ae4ffdeaf73e07ce20dc420e4b1ca0eb347f50bf95"
SUPERVISOR_SHA = "f01f30472e863a63d83e74bf52fca259a1371fa551a99ae470c2edeb85b22df2"


def require(ok, message):
    if not ok:
        raise ValueError(message)


def sha(path):
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def module(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    loaded = importlib.util.module_from_spec(spec)
    sys.modules[name] = loaded
    spec.loader.exec_module(loaded)
    return loaded


def main():
    cli = argparse.ArgumentParser(description=__doc__)
    for option in ("raw", "primary-raw", "project", "output"):
        cli.add_argument("--" + option, type=Path, required=True)
    args = cli.parse_args()
    roots = {"legacy": args.raw.resolve(), "primary": args.primary_raw.resolve(),
             "project": args.project.resolve()}
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    inventory = {}

    def record(kind, relative):
        path = roots[kind] / relative
        key = kind + "/" + str(relative)
        if key not in inventory:
            inventory[key] = dict(root=kind, path=str(relative), sha256=sha(path), bytes=path.stat().st_size)
        return path

    def read(kind, relative):
        return json.loads(record(kind, relative).read_text())

    def pinned(kind, relative, original):
        path = record(kind, relative)
        require(inventory[kind + "/" + str(relative)]["sha256"] == plan["files_sha256"][str(original)],
                "registered file differs: " + kind + "/" + str(relative))
        return path

    plan = read("legacy", "preregistration.json")
    require(sha(roots["legacy"] / "preregistration.json") == PREREGISTRATION_SHA, "legacy preregistration differs")
    require(plan["fixture_order"] == FIXTURES and plan["widths"] == WIDTHS and plan["repeats"] == 3
            and plan["expected_rows"] == 48 and plan["slots"] == 8 and plan["backend"] == BACKEND,
            "fixed matrix differs")
    order = [dict(fixture=f, width=w, repeat=r) for f in FIXTURES for w in WIDTHS for r in (1, 2, 3)]
    require(plan["row_order"] == order and [p["name"] for p in plan["phases"]] == FIXTURES, "row order differs")
    old_root = PurePosixPath(plan["provenance"]).parent
    old_project = PurePosixPath(plan["project"])
    helpers = {"legacy": "benchmarks/overlay-runtime/bench.py", "launcher": "benchmarks/overlay-runtime/launch.py",
               "shared": "benchmarks/pi-runtime/bench.py"}
    for relative in helpers.values():
        pinned("project", relative, old_project / relative)
    # The shared helper reads this JSON at import; its contents do not enter summarize().
    record("project", "benchmarks/pi-runtime/pi-pin.json")
    module("launch", roots["project"] / helpers["launcher"])
    legacy = module("legacy_verify", roots["project"] / helpers["legacy"])
    runtime = legacy.shared(roots["project"])
    require(plan["runtime_base"] == legacy.BASE and plan["runtime_objects"] == legacy.OBJECTS,
            "historical runtime identity differs")

    for relative in ("tmlegacy-provenance.json", "storage-identity.json", "run-serial.py"):
        pinned("legacy", relative, old_root / relative)
    provenance = read("legacy", "tmlegacy-provenance.json")
    storage = read("legacy", "storage-identity.json")
    for actual, wanted in ((provenance["runtime_base"], plan["runtime_base"]),
                           (provenance["runtime_objects"], plan["runtime_objects"]),
                           (provenance["template_main_commit"], plan["template_commit"]),
                           (provenance["bridge_commit"], plan["bridge_commit"]),
                           (provenance["binary_sha256"], plan["binary_sha256"])):
        require(actual == wanted, "provenance identity differs")
    require(storage["parent_overlay_mounts"] == [] and storage["euid"] == 0, "storage admission differs")
    original_primary = PurePosixPath(plan["phases"][0]["main_registration"]).parents[1]
    pinned("primary", "preregistration.json", original_primary / "preregistration.json")
    primary = read("primary", "preregistration.json")
    require(primary["widths"] == WIDTHS, "primary widths differ")

    supervision = read("legacy", "supervision-registration.json")
    proof = read("legacy", "supervisor.json")
    driver = read("legacy", "driver-status.json")
    require(supervision["original_registration_sha256"] == PREREGISTRATION_SHA
            and supervision["supervisor_sha256"] == SUPERVISOR_SHA, "supervision registration differs")
    command = supervision["argv"][supervision["argv"].index("--") + 1:]
    require(proof["command"] == command == ["/usr/bin/python3", str(old_root / "run-serial.py"),
            "--registration-sha256", PREREGISTRATION_SHA, "--execute"], "supervised driver differs")
    require(proof["complete"] is True and proof["exit_code"] == 0 and proof["runner"]["returncode"] == 0
            and not proof["error"] and not proof["stop_signal"], "outer supervision incomplete or failed")
    require(proof["subreaper"] is True and proof["poll_seconds"] == supervision["poll_seconds"] == 0.02
            and proof["term_seconds"] == 2.0 and proof["kill_seconds"] == 5.0, "continuous supervisor method differs")
    cleanup = proof["cleanup"]
    require(cleanup["trigger"] == "runner_exit" and cleanup["clean"] is True
            and not any(cleanup[k] for k in ("live_descendants", "signals", "remaining", "errors")),
            "outer supervisor required live-leftover cleanup or retained errors")
    require(driver["preregistration_sha256"] == PREREGISTRATION_SHA and driver["serial"] is True
            and driver["complete"] is True and driver["expected_rows"] == driver["completed_rows"] == 48
            and "stopped_on_failure" not in driver, "driver incomplete or failed")
    require([p["name"] for p in driver["phases"]] == FIXTURES, "driver phases differ")

    captured = []
    legacy.emit = lambda _path, value: captured.append(value)
    summaries, row_inventory = {}, []
    for phase, finished in zip(plan["phases"], driver["phases"]):
        fixture = phase["name"]
        require(finished["argv"] == phase["argv"] and finished["exit_code"] == 0
                and finished["completed_rows"] == phase["expected_rows"] == 24, "phase completion differs")
        pinned("primary", fixture + "/registration.json", phase["main_registration"])
        main_reg = read("primary", fixture + "/registration.json")
        reg = read("legacy", fixture + "/registration.json")
        require(reg["version"] == 1 and reg["backend"] == BACKEND and reg["euid"] == 0
                and reg["widths"] == WIDTHS and reg["repeats"] == 3 and reg["expected_rows"] == 24
                and reg["slots"] == 8 and reg["argv"] == phase["argv"][4:], "phase registration differs")
        require(reg["main_registration_sha256"] == phase["main_registration_sha256"]
                and reg["main_matrix_commit"] == main_reg["threadmill_commit"] == phase["main_matrix_commit"],
                "primary registration link differs")
        require(reg["fixture_commit"] == phase["fixture_commit"] == main_reg["fixture_commit"]
                == primary["fixtures"][fixture]["commit"], "fixture identity differs")
        require(reg["traces"] == main_reg["traces"] == primary["fixtures"][fixture]["traces"], "primary trace set differs")
        require(reg["provenance"] == provenance and reg["provenance_sha256"] == sha(roots["legacy"] / "tmlegacy-provenance.json"),
                "phase provenance differs")
        require(reg["harness_sha256"] == {name: sha(roots["project"] / relative) for name, relative in helpers.items()},
                "phase helper identity differs")
        lower = storage["lowers"][fixture]
        backing = lower["backing"]
        require(reg["lower_backing"] == backing and backing["kind"] == "loop_image" and backing["block_readonly"] is True
                and backing["image"] == phase["lower_image"] and lower["image_sha256"] == phase["lower_image_sha256"],
                "lower backing identity differs")
        for field, identity in (("lower", lower), ("upper", storage["upper"])):
            mount = json.loads(reg[field + "_mount"])["filesystems"][0]
            expected = identity["mount"]["filesystems"][0]
            require(all(mount[k] == expected[k] for k in ("target", "source", "fstype")), field + " mount identity differs")
            require(mount["fstype"] == ("ext4" if field == "lower" else "btrfs"), field + " filesystem differs")
            if field == "lower":
                require("ro" in mount["options"].split(","), "lower is not recorded readonly")
        after = read("legacy", fixture + "/lower-after.json")
        for field in ("df_used_bytes", "fixture_du_raw"):
            require(reg["lower_before"][field] == after[field] == lower["physical_registration_sample"][field],
                    "fixed lower allocation differs: " + field)

        rows = []
        for width in WIDTHS:
            relative_trace = f"{fixture}/trace-{width}.json"
            trace = pinned("primary", relative_trace, phase["traces"][str(width)]["path"])
            require(sha(trace) == phase["traces"][str(width)]["sha256"] == reg["traces"][str(width)], "trace identity differs")
            for repeat in (1, 2, 3):
                label = f"{fixture}/{BACKEND}-w{width}-r{repeat}"
                inputs = {name: roots["legacy"] / (label + suffix + ".json") for name, suffix in
                          (("replay", ".replay"), ("measured", ".measured"), ("audit", ".audit"),
                           ("replay_launch", ".replay-launch"), ("audit_launch", ".audit-launch"))}
                captured.clear()
                code = legacy.verify(argparse.Namespace(trace=trace, **inputs, json_out=output / "unused-capture.json"))
                require(len(captured) == 1, "verify-row did not emit exactly one row")
                verified = captured.pop()
                verified.update(repeat=repeat, formal=True)
                saved = read("legacy", label + ".json")
                require(verified == saved, "recomputed final row differs: " + label)
                require(not saved["method_errors"] and saved["failure_kind"] in ("none", "capacity")
                        and code == int(bool(saved["errors"])), "method failure: " + label)
                require(saved["backend"] == BACKEND and saved["agents"] == width, "final row identity differs: " + label)
                missing = []
                for name, path in inputs.items():
                    if not path.exists():
                        require(saved["operation_errors"] > 0 and name in ("audit", "audit_launch"),
                                "missing required evidence: " + str(path))
                        missing.append(name)
                        continue
                    record("legacy", path.relative_to(roots["legacy"]))
                    if name.endswith("launch"):
                        launch = json.loads(path.read_text())
                        separate = read("legacy", str(path.relative_to(roots["legacy"])) + ".namespace.json")
                        require(launch["namespace"] == separate and separate["lower_backing"] == backing,
                                "namespace/backing proof differs: " + label)
                row_inventory.append(dict(fixture=fixture, agents=width, repeat=repeat, path=label + ".json",
                                          errors=saved["errors"], operation_errors=saved["operation_errors"],
                                          failure_kind=saved["failure_kind"], absent_capacity_audit_files=missing))
                # summarize() needs counters/latencies, not the large operation/sample arrays.
                rows.append({k: v for k, v in saved.items() if k not in ("operations", "checkpoint_ids", "physical_disk")})
                rows[-1]["physical_disk"] = {k: saved["physical_disk"][k] for k in ("peak_delta_bytes", "retained_delta_bytes")}
        capacity = runtime.summarize(rows, 3, terminal_errors=legacy.terminal_errors)
        summary = read("legacy", fixture + "/summary.json")
        require(summary["complete"] is True and summary["metadata"] == reg and summary["lower_after"] == after
                and summary["capacity"] == capacity, "saved phase summary differs: " + fixture)
        summaries[fixture] = dict(rows_verified=len(rows), capacity=capacity,
                                  failure_kinds=dict(Counter(row["failure_kind"] for row in rows)),
                                  operation_errors=sum(row["operation_errors"] for row in rows),
                                  lower_before=reg["lower_before"], lower_after=after, lower_backing=backing)

    require(len(row_inventory) == 48 and [dict(fixture=r["fixture"], width=r["agents"], repeat=r["repeat"])
                                       for r in row_inventory] == order, "complete registered matrix differs")
    result = dict(rows_verified=48, historical_only=True, audit_script_sha256=sha(Path(__file__)),
                  preregistration_sha256=PREREGISTRATION_SHA, normal_reaped=len(proof["normal_reaped"]),
                  summaries=summaries, layout_caveat=plan["layout_caveat"],
                  evidence_limits=["Saved checkpoint restore proofs are checked; no restore is re-executed.",
                                   "Lower images and the tmlegacy binary are not rehashed; their registered identity and recorded admission are checked.",
                                   "Lower backing matches storage admission and every saved namespace proof; the final backing check is attested by the complete frozen matrix, not a separate saved after-backing snapshot.",
                                   "P50/P95 are the frozen helper's saved per-run statistics; this audit does not recompute operation percentiles."])
    (output / "inventory.json").write_text(json.dumps(dict(files=list(inventory.values()), rows=row_inventory), indent=2) + "\n")
    (output / "summary.json").write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps(dict(rows_verified=48, historical_only=True, capacity_failures=sum(r["failure_kind"] == "capacity" for r in row_inventory))))


if __name__ == "__main__":
    main()
