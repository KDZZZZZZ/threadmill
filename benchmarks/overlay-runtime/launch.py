#!/usr/bin/env python3
"""Fixed legacy benchmark launcher."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import time


def emit(path, value):
    with Path(path).open("x") as output:
        json.dump(value, output, indent=2)
        output.write("\n")


def lower_backing(lower, upper):
    device = os.stat(lower).st_dev
    block = Path(f"/sys/dev/block/{os.major(device)}:{os.minor(device)}")
    loop = block / "loop/backing_file"
    if not loop.exists() or (block / "ro").read_text().strip() != "1":
        raise RuntimeError("requires this group's dedicated readonly ext4 loop filesystem; shared host bind is unsupported")
    if not os.statvfs(lower).f_flag & os.ST_RDONLY:
        raise RuntimeError("dedicated lower loop filesystem must be mounted readonly")
    image = Path("/" + loop.read_text().strip().lstrip("/")).resolve()
    info = image.stat()
    if info.st_dev == os.stat(upper).st_dev:
        raise RuntimeError("lower image backing must be outside the dedicated upper filesystem")
    return {"kind": "loop_image", "loop_device": device, "block_readonly": True,
            "image": str(image), "device": info.st_dev, "inode": info.st_ino,
            "allocated_bytes": info.st_blocks * 512, "scope": "backing file allocated blocks; excludes backing filesystem metadata"}


def overlay_mounts(root):
    found = []
    for line in Path("/proc/self/mountinfo").read_text().splitlines():
        left, right = line.split(" - ", 1)
        name = re.sub(r"\\([0-7]{3})", lambda match: chr(int(match[1], 8)), left.split()[4])
        if right.split()[0] == "overlay" and (name == str(root) or name.startswith(str(root) + "/")):
            found.append(name)
    return found


def children(pid):
    try:
        return [int(value) for value in Path(f"/proc/{pid}/task/{pid}/children").read_text().split()]
    except (FileNotFoundError, ProcessLookupError):
        return []


def start_time(pid):
    try:
        return Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()[19]
    except (FileNotFoundError, ProcessLookupError):
        return None


def read_namespace(args):
    paths = [args.binary, args.trace, args.repo, args.lower, Path(__file__).resolve()]
    if args.audit_only:
        paths.append(args.replay_report)
    for path in dict.fromkeys(paths):
        subprocess.run(["/usr/bin/mount", "--bind", path, path], check=True)
        subprocess.run(["/usr/bin/mount", "-o", "remount,bind,ro", path], check=True)
    if hashlib.sha256(args.binary.read_bytes()).hexdigest() != args.binary_sha256:
        raise RuntimeError("registered binary differs")
    if hashlib.sha256(args.trace.read_bytes()).hexdigest() != args.trace_sha256:
        raise RuntimeError("registered trace differs")
    return all(os.statvfs(path).f_flag & os.ST_RDONLY for path in paths)


def inside(args):
    state = {"version": 1, "euid": os.geteuid(), "pid": os.getpid(),
             "mount_namespace": os.readlink("/proc/self/ns/mnt"), "pid_namespace": os.readlink("/proc/self/ns/pid"),
             "parent_mount_namespace": os.environ["TMLEGACY_PARENT_MNT"], "readonly_sources": False,
             "binary_exit": None, "overlay_mounts_after": None, "children_after": None}
    try:
        state["lower_backing"] = lower_backing(args.lower, args.live_root.parent)
        state["readonly_sources"] = read_namespace(args)
        if not state["readonly_sources"] or state["mount_namespace"] == state["parent_mount_namespace"]:
            raise RuntimeError("source readonly/private mount proof failed")
        state["cap_eff"] = next(line.split()[1] for line in Path("/proc/self/status").read_text().splitlines() if line.startswith("CapEff:"))
        # Keep namespace init alive until the parent records its host PID.
        # This handshake is outside the adapter's measured wall boundary.
        ready = Path(str(args.evidence_out) + ".parent-ready")
        deadline = time.monotonic() + 5
        while not ready.exists() and time.monotonic() < deadline:
            time.sleep(0.01)
        if not ready.exists():
            raise RuntimeError("parent PID proof handshake timed out")
        argv = [args.binary, "-trace-in", args.trace, "-repo", args.repo, "-lower", args.lower,
                "-live-root", args.live_root, "-slots", str(args.slots), "-json-out", args.json_out]
        if args.audit_only:
            argv += ["-audit-only", "-replay-report", args.replay_report]
        environment = {"PATH": "/usr/bin:/bin", "LANG": "C.UTF-8", "TMPDIR": os.environ.get("TMPDIR", str(args.live_root.parent / "tmp")),
                       "GIT_OPTIONAL_LOCKS": "0", "GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "safe.directory",
                       "GIT_CONFIG_VALUE_0": str(args.repo)}
        state["binary_exit"] = subprocess.run(["/usr/bin/setpriv", "--pdeathsig", "KILL", *argv], env=environment).returncode
        # These proofs are saved while the namespace is still alive. Teardown
        # is a fallback, never a replacement for the runtime's own Close.
        state["overlay_mounts_after"] = overlay_mounts(args.live_root)
        state["children_after"] = children(os.getpid())
        emit(str(args.evidence_out) + ".namespace.json", state)
        if state["overlay_mounts_after"] or state["children_after"]:
            return 1
        return state["binary_exit"]
    except Exception as error:
        state["error"] = str(error)
        emit(str(args.evidence_out) + ".namespace.json", state)
        return 1


def supervise(args):
    state = {"version": 1, "supervisor_pid": os.getpid(), "signal": 0, "errors": 0,
             "process_exit": None, "known_children_gone": False, "parent_overlay_mounts_after": None}
    process = None
    known = {}

    def interrupted(signum, _frame):
        state["signal"] = signum
        if process is not None:
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass

    signal.signal(signal.SIGINT, interrupted)
    signal.signal(signal.SIGTERM, interrupted)
    try:
        parent_namespace = os.readlink("/proc/self/ns/mnt")
        environment = {**os.environ, "TMLEGACY_PARENT_MNT": parent_namespace}
        argv = ["/usr/bin/setpriv", "--pdeathsig", "KILL", "/usr/bin/unshare", "--mount", "--pid", "--fork",
                "--mount-proc", "--propagation", "private", "--kill-child=KILL", "--",
                sys.executable, Path(__file__).resolve(), "--inside", *sys.argv[1:]]
        process = subprocess.Popen([str(arg) for arg in argv], env=environment, start_new_session=True)
        state["unshare_pid"] = process.pid
        known[process.pid] = start_time(process.pid)
        while process.poll() is None:
            for pid in children(process.pid):
                if pid not in known:
                    known[pid] = start_time(pid)
            if len(known) >= 2 and not Path(str(args.evidence_out) + ".parent-ready").exists():
                Path(str(args.evidence_out) + ".parent-ready").touch(exist_ok=False)
            if state["signal"]:
                interrupted(state["signal"], None)
            time.sleep(0.02)
        state["process_exit"] = process.wait()
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline and any(start is not None and start_time(pid) == start for pid, start in known.items()):
            time.sleep(0.02)
        state["known_pid_start_times"] = known
        state["known_children_gone"] = len(known) >= 2 and all(start is not None and start_time(pid) != start for pid, start in known.items())
        state["parent_overlay_mounts_after"] = overlay_mounts(args.live_root)
        state["namespace"] = json.loads(Path(str(args.evidence_out) + ".namespace.json").read_text())
        if not state["known_children_gone"] or state["parent_overlay_mounts_after"]:
            raise RuntimeError("process tree or parent mount cleanup proof failed")
    except Exception as error:
        state["errors"] += 1
        state["error"] = str(error)
    finally:
        if process is not None and process.poll() is None:
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            process.wait()
        emit(args.evidence_out, state)
    return 1 if state["errors"] or state["signal"] else state["process_exit"]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ["binary", "trace", "repo", "lower", "live-root", "json-out", "evidence-out"]:
        parser.add_argument("--" + name, type=Path, required=True)
    parser.add_argument("--binary-sha256", required=True)
    parser.add_argument("--trace-sha256", required=True)
    parser.add_argument("--slots", type=int, default=8)
    parser.add_argument("--audit-only", action="store_true")
    parser.add_argument("--replay-report", type=Path)
    parser.add_argument("--inside", action="store_true", help=argparse.SUPPRESS)
    args = parser.parse_args()
    if os.geteuid() != 0 or args.slots < 1 or (args.audit_only and args.replay_report is None):
        parser.error("requires the root benchmark runner, positive slots, and audit replay report")
    for name in ["binary", "trace", "repo", "lower", "live_root", "json_out", "evidence_out", "replay_report"]:
        path = getattr(args, name)
        if path is not None:
            if not path.is_absolute() or path == Path("/"):
                parser.error("fixed source/output paths must be absolute and below root")
            setattr(args, name, path.resolve())
    return inside(args) if args.inside else supervise(args)


if __name__ == "__main__":
    sys.exit(main())
