"""Model-free preflight using Harbor's actual image-layer to VFS-volume mounts."""

from __future__ import annotations

import argparse
import asyncio
import hashlib
import json
import os
import platform
import re
import shlex
import subprocess
import tempfile
import uuid
from datetime import datetime, timezone
from pathlib import Path
from types import SimpleNamespace

from benchmarks.harbor.threadmill_agent import Threadmill, _require_static_tracer


def _command(*args: str) -> str:
    result = subprocess.run(args, capture_output=True, text=True, timeout=120)
    if result.returncode:
        raise RuntimeError(result.stderr.strip() or f"{args[0]} exited {result.returncode}")
    return result.stdout.strip()


class _Container:
    def __init__(self, name: str) -> None:
        self.name = name

    async def upload_file(self, source, destination):
        _command("docker", "cp", str(source), f"{self.name}:{destination}")

    async def exec(self, command, user=None, env=None, cwd=None, timeout_sec=None):
        args = ["docker", "exec"]
        if user is not None:
            args.extend(["--user", str(user)])
        for key, value in (env or {}).items():
            args.extend(["--env", f"{key}={value}"])
        if cwd:
            args.extend(["--workdir", cwd])
        result = subprocess.run([*args, self.name, "bash", "-c", command],
                                capture_output=True, text=True, timeout=timeout_sec or 120)
        return SimpleNamespace(stdout=result.stdout, stderr=result.stderr,
                               return_code=result.returncode)


def check(*, binary: Path, tracer: Path | None, image: str | None,
          workspace: str, runtime: str) -> dict:
    report = {"utc": datetime.now(timezone.utc).isoformat(), "kernel": platform.release(),
              "cpu_count": os.cpu_count(), "workspace": workspace, "failures": []}
    failures = report["failures"]
    for name, args in (("runtime_version", [runtime, "--version"]),
                       ("go_version", ["go", "version"])):
        try:
            report[name] = _command(*args)
        except (OSError, RuntimeError, subprocess.TimeoutExpired) as error:
            failures.append(f"{name}: {error}")
    try:
        info = json.loads(_command("docker", "info", "--format", "{{json .}}"))
        report["docker"] = {key: info.get(key) for key in
                            ("ID", "ServerVersion", "Driver", "DockerRootDir", "DriverStatus",
                             "NCPU", "MemTotal", "CgroupVersion")}
        if info.get("Driver") != "btrfs":
            failures.append(f"docker_storage_driver: expected btrfs, got {info.get('Driver')}")
        root_fs = _command("findmnt", "-n", "-o", "FSTYPE", "-T", info["DockerRootDir"])
        report["docker_data_root_filesystem"] = root_fs
        if root_fs != "btrfs":
            failures.append(f"docker_data_root_filesystem: expected btrfs, got {root_fs}")
    except (OSError, RuntimeError, KeyError, ValueError, subprocess.TimeoutExpired) as error:
        failures.append(f"docker: {error}")
    try:
        _require_static_tracer(tracer)
        assert tracer is not None
        report["tracer_sha256"] = hashlib.sha256(tracer.read_bytes()).hexdigest()
        report["strace_version"] = _command(str(tracer), "-V").splitlines()[0]
        match = re.search(r"version (\d+)\.(\d+)", report["strace_version"])
        if not match or tuple(map(int, match.groups())) < (5, 3):
            raise ValueError("strace >= 5.3 required")
    except (OSError, ValueError, RuntimeError, subprocess.TimeoutExpired) as error:
        failures.append(f"static_strace: {error}")
        report["ok"] = False
        return report
    if not binary.is_file() or not image:
        failures.append("container_probe: provide --binary and --image (or THREADMILL_BENCH_DOCTOR_IMAGE)")
        report["ok"] = False
        return report

    name = "threadmill-doctor-" + uuid.uuid4().hex[:12]
    volume = name + "-vfs"
    try:
        inspected = json.loads(_command("docker", "image", "inspect", image))[0]
        report["image"] = {key: inspected.get(key) for key in ("Id", "RepoDigests", "Architecture", "Os")}
        report["image"]["User"] = (inspected.get("Config") or {}).get("User", "")
        report["binary_sha256"] = hashlib.sha256(binary.read_bytes()).hexdigest()
        _command("docker", "create", "--name", name, "--network", "none", "--cap-add", "SYS_ADMIN",
                 "--security-opt", "apparmor=unconfined", "--mount",
                 f"type=volume,source={volume},target=/threadmill-vfs",
                 "--entrypoint", "/bin/bash", inspected["Id"], "-c", "sleep 600")
        _command("docker", "start", name)
        _command("docker", "exec", "--user", "root", name, "bash", "-c",
                 f"mkdir -p {shlex.quote(workspace)} /installed-agent /usr/local/bin /logs/agent")
        environment = _Container(name)
        with tempfile.TemporaryDirectory(prefix="threadmill-doctor-") as directory:
            agent = Threadmill(Path(directory), binary=binary, tracer=tracer,
                               model_name="openai/preflight", workspace=workspace)
            try:
                asyncio.run(agent.install(environment))
            finally:
                try:
                    report["container_setup"] = _command("docker", "exec", name, "cat", "/logs/agent/threadmill/setup.txt")
                except (OSError, RuntimeError, subprocess.TimeoutExpired) as error:
                    failures.append(f"container_setup: {error}")
            config = Path(directory) / "config.json"
            config.write_text(json.dumps({"exec": {"external_sandbox": True,
                               "external_workspace_isolation": True, "require_dependency_tracing": True,
                               "cache": {"enabled": True}}, "vfs": {"live_root": "/threadmill-vfs"}}))
            _command("docker", "cp", str(config), f"{name}:/tmp/doctor.json")
            report["runtime_identity"] = _command("docker", "exec", name, "id")
            report["runtime_preflight"] = json.loads(_command(
                "docker", "exec", "--env", "HOME=/tmp/threadmill-doctor-home",
                "--env", "TMPDIR=/tmp", name, "/installed-agent/threadmill",
                "-C", workspace, "-config", "/tmp/doctor.json", "-exec-doctor"))
            if report["runtime_preflight"].get("exec_dependency_tracing") is not True:
                failures.append("exec_dependency_tracing: unavailable")
            if report["runtime_preflight"].get("exec_dependency_tracing_enabled") is not True:
                failures.append("exec_dependency_tracing_enabled: unavailable")
    except (OSError, RuntimeError, ValueError, KeyError, subprocess.TimeoutExpired) as error:
        failures.append(f"container_probe: {error}")
    finally:
        for args in (("docker", "rm", "--force", name), ("docker", "volume", "rm", volume)):
            try:
                _command(*args)
            except (OSError, RuntimeError, subprocess.TimeoutExpired) as error:
                if "no such volume" not in str(error).lower() and "no such container" not in str(error).lower():
                    failures.append(f"cleanup: {error}")
    report["ok"] = not failures
    return report


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--tracer", type=Path, default=os.environ.get("THREADMILL_STRACE_BINARY"))
    parser.add_argument("--image", default=os.environ.get("THREADMILL_BENCH_DOCTOR_IMAGE"))
    parser.add_argument("--workspace", default=os.environ.get("THREADMILL_BENCH_WORKSPACE", "/workspace/repo"))
    parser.add_argument("--runtime", default=os.environ.get("THREADMILL_HARBOR_BIN", "harbor"))
    args = parser.parse_args()
    report = check(**vars(args))
    print(json.dumps(report, indent=2, ensure_ascii=False))
    return 0 if report["ok"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
