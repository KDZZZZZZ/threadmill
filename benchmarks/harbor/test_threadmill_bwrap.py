"""Process-boundary regressions; actual UID/namespace admission stays in doctor."""

from __future__ import annotations

import asyncio
from collections import deque
import json
from pathlib import Path, PurePosixPath
import shlex
import struct
import subprocess
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import yaml

from harbor.agents.installed.base import NonZeroAgentExitCodeError
from harbor.agents.model_connection import ResolvedModelConnection
from harbor.models.agent.context import AgentContext
from benchmarks.harbor import threadmill_agent


def _result(stdout="", stderr="", code=0):
    return SimpleNamespace(stdout=stdout, stderr=stderr, return_code=code)


_INSTALL_RESULTS = (
    _result("2000\n2000\n"),
    _result("mount_namespace=no\nrepo_to_vfs_reflink=yes\nstrace_version=6.8\nstrace_probe=yes\n"),
    _result("task_repo_to_vfs_reflink=yes\ntask_directory_write=yes\n"),
)


class _ProcessBoundary:
    # None means the pinned image's USER, not an override to root.
    default_user = None

    def __init__(self, *results):
        self.results = deque(results)
        self.calls = []
        self.uploads = {}
        self.last_result = None

    async def upload_file(self, source, destination):
        self.uploads[str(destination)] = Path(source).read_bytes()

    async def exec(self, command, user=None, **kwargs):
        self.calls.append({"command": command, "user": user, **kwargs})
        response = self.results.popleft()
        self.last_result = response(command) if callable(response) else response
        return self.last_result


class _ConnectedThreadmill(threadmill_agent.Threadmill):
    @property
    def model_connection(self):
        return ResolvedModelConnection(api_key="fixture-key", base_url="https://example.test/v1")


class ThreadmillBwrapTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.binary = self.root / "threadmill"
        self.binary.touch()
        self.tracer = self.root / "strace"
        elf = bytearray(120)
        elf[:16] = b"\x7fELF\x02\x01\x01" + b"\x00" * 9
        struct.pack_into("<HHIQQQIHHHHHH", elf, 16, 2, 62, 1, 0, 64, 0, 0, 64, 56, 1, 0, 0, 0)
        struct.pack_into("<IIQQQQQQ", elf, 64, 1, 5, 0, 0, 0, len(elf), len(elf), 4096)
        self.tracer.write_bytes(elf)

    def agent(self, **kwargs):
        return _ConnectedThreadmill(
            self.root / "logs", binary=self.binary, tracer=self.tracer,
            model_name="openai/model", **kwargs,
        )

    def test_bwrap_is_explicit_and_external_remains_the_default(self):
        for backend in ("external", "bwrap"):
            with self.subTest(backend=backend):
                install = list(_INSTALL_RESULTS)
                if backend == "external":
                    install[1] = _result(
                        "mount_namespace=yes\nrepo_to_vfs_reflink=yes\n"
                        "strace_version=6.8\nstrace_probe=yes\n"
                    )
                environment = _ProcessBoundary(
                    *install, _result(), _result(), _result(),
                    _result(json.dumps({
                        "exec_dependency_tracing": True, "exec_dependency_tracing_enabled": True,
                        "exec_sandbox_backend": backend,
                    })), _result(),
                )
                kwargs = {} if backend == "external" else {"exec_backend": "bwrap"}
                agent = self.agent(cache_mode="shadow", **kwargs)
                asyncio.run(agent.install(environment))
                asyncio.run(agent.run("do it", environment, AgentContext()))
                uploaded = yaml.safe_load(environment.uploads["/tmp/threadmill-agent/config.yaml"])
                self.assertEqual(uploaded["exec"]["external_sandbox"], backend == "external")
                self.assertEqual(uploaded["exec"]["external_workspace_isolation"], backend == "external")
                self.assertTrue(uploaded["exec"]["require_dependency_tracing"])
                self.assertIsNone(environment.calls[-2]["user"])
                self.assertIsNone(environment.calls[-1]["user"])
                self.assertIn("-exec-doctor", environment.calls[-2]["command"])
                self.assertIn("-p 'do it'", environment.calls[-1]["command"])

    def test_default_external_still_requires_mount_namespace(self):
        environment = _ProcessBoundary(*_INSTALL_RESULTS[:2])
        with self.assertRaisesRegex(RuntimeError, "mount_namespace"):
            asyncio.run(self.agent().install(environment))
        self.assertIsNone(environment.calls[0]["user"])
        self.assertEqual(environment.calls[-1]["user"], "root")

    def test_private_files_belong_to_the_image_user_and_are_readable_by_that_user(self):
        environment = _ProcessBoundary(
            *_INSTALL_RESULTS, _result(), _result(), _result(),
            _result(json.dumps({
                "exec_dependency_tracing": True, "exec_dependency_tracing_enabled": True,
                "exec_sandbox_backend": "bwrap",
            })), _result(),
        )
        agent = self.agent(exec_backend="bwrap", cache_mode="shadow", config={"exec": {"slots": 2}})
        asyncio.run(agent.install(environment))
        asyncio.run(agent.run("do it", environment, AgentContext()))

        paths = {
            "/tmp/threadmill-agent/config.yaml",
            "/tmp/threadmill-agent-home/.threadmill/config.yaml",
            "/tmp/threadmill-agent-home/.threadmill/credentials.yaml",
        }
        self.assertTrue(paths.issubset(environment.uploads))
        self.assertEqual(
            yaml.safe_load(environment.uploads["/tmp/threadmill-agent-home/.threadmill/credentials.yaml"]),
            {"harbor": "fixture-key"},
        )
        self.assertEqual(
            yaml.safe_load(environment.uploads["/tmp/threadmill-agent-home/.threadmill/config.yaml"]),
            {"exec": {"slots": 2}},
        )
        permissions = next(call for call in environment.calls
                           if call["user"] == "root" and "/credentials.yaml" in call["command"])
        self.assertEqual(permissions["user"], "root")
        commands = [shlex.split(part) for part in permissions["command"].split(";")]
        owner = next(parts[1:] for parts in commands if parts and parts[0] == "chown")
        mode = next(parts[1:] for parts in commands if parts and parts[0] == "chmod")
        self.assertEqual(owner[0], "2000:2000")
        self.assertEqual(set(owner[1:]), paths)
        self.assertEqual(int(mode[0], 8), 0o600)
        self.assertEqual(set(mode[1:]), paths)
        self.assertNotIn("-R", owner)
        self.assertIsNone(environment.calls[0]["user"])
        self.assertIsNone(environment.calls[2]["user"])
        readable = next(call for call in environment.calls if "test -r" in call["command"])
        self.assertIsNone(readable["user"])
        self.assertIn("test -r", readable["command"])
        self.assertTrue(all(path in readable["command"] for path in paths))
        self.assertNotIn("fixture-key", readable["command"])
        self.assertIsNone(environment.calls[-2]["user"])
        self.assertIsNone(environment.calls[-1]["user"])

    def test_unreadable_private_configuration_stops_before_doctor_or_model(self):
        environment = _ProcessBoundary(
            *_INSTALL_RESULTS, _result(), _result(),
            _result(stderr="Permission denied reading private configuration", code=13),
        )
        agent = self.agent(exec_backend="bwrap")
        asyncio.run(agent.install(environment))
        with self.assertRaisesRegex(NonZeroAgentExitCodeError, "exit 13"):
            asyncio.run(agent.run("do it", environment, AgentContext()))
        self.assertIsNone(environment.calls[-1]["user"])
        self.assertIn("test -r", environment.calls[-1]["command"])
        self.assertFalse(any("-exec-doctor" in call["command"] for call in environment.calls))
        self.assertFalse(any("console.log" in call["command"] for call in environment.calls))

    def test_requested_bwrap_cannot_accept_an_external_runtime(self):
        diagnostic = {
            "exec_dependency_tracing": True,
            "exec_dependency_tracing_enabled": True,
            "exec_sandbox_backend": "external",
            "exec_workspace_isolation": "mount_namespace",
        }
        environment = _ProcessBoundary(
            *_INSTALL_RESULTS, _result(), _result(), _result(),
            _result(json.dumps(diagnostic)),
        )
        agent = self.agent(exec_backend="bwrap", cache_mode="shadow")
        asyncio.run(agent.install(environment))
        with self.assertRaisesRegex(RuntimeError, "requested bwrap backend"):
            asyncio.run(agent.run("do it", environment, AgentContext()))
        doctor = environment.calls[-1]
        self.assertIsNone(doctor["user"])
        self.assertIn("-exec-doctor", doctor["command"])
        self.assertEqual(doctor["env"]["HOME"], "/tmp/threadmill-agent-home")
        self.assertEqual(doctor["env"]["TMPDIR"], "/tmp/threadmill-agent-tmp")
        self.assertNotIn("-p ", doctor["command"])

    def test_full_bwrap_capability_accepts_the_existing_cwd_label(self):
        environment = _ProcessBoundary(
            *_INSTALL_RESULTS, _result(), _result(), _result(),
            _result(json.dumps({
                "exec_dependency_tracing": True, "exec_dependency_tracing_enabled": True,
                "exec_sandbox_backend": "bwrap", "exec_workspace_isolation": "cwd",
            })), _result(),
        )
        agent = self.agent(exec_backend="bwrap", cache_mode="shadow")
        asyncio.run(agent.install(environment))
        asyncio.run(agent.run("do it", environment, AgentContext()))
        self.assertIsNone(environment.calls[-1]["user"])
        self.assertIn("-p 'do it'", environment.calls[-1]["command"])
        self.assertEqual(environment.calls[-1]["env"], environment.calls[-2]["env"])

    def task_paths(self, directory, bwrap_script):
        root = self.root / directory
        paths = {
            "_REMOTE_BWRAP": root / "bwrap",
            "_REMOTE_TRACER": root / "strace",
            "_REMOTE_LOGS": root / "logs",
            "_REMOTE_VFS": root / "vfs",
            "_REMOTE_HOME": root / "home",
            "_REMOTE_TMP": root / "tmp",
            "_REMOTE_CONFIG": root / "config" / "config.yaml",
            "_REMOTE_USER_CONFIG": root / "home" / ".threadmill" / "config.yaml",
            "_REMOTE_CREDENTIALS": root / "home" / ".threadmill" / "credentials.yaml",
        }
        for name, path in paths.items():
            (path.parent if name in {"_REMOTE_BWRAP", "_REMOTE_TRACER", "_REMOTE_CONFIG", "_REMOTE_USER_CONFIG",
                                    "_REMOTE_CREDENTIALS"} else path).mkdir(parents=True, exist_ok=True)
        paths["_REMOTE_TRACER"].write_text("#!/bin/sh\nexit 0\n")
        paths["_REMOTE_TRACER"].chmod(0o755)
        bwrap = paths["_REMOTE_BWRAP"]
        bwrap.write_text("#!/bin/sh\n" + bwrap_script)
        bwrap.chmod(0o755)
        workspace = root / "workspace"
        workspace.mkdir()
        return paths, bwrap, workspace

    @staticmethod
    def shell(prefix=""):
        def execute(command):
            completed = subprocess.run(
                ["bash", "-c", prefix + command], capture_output=True, text=True, timeout=10,
            )
            return _result(completed.stdout, completed.stderr, completed.returncode)
        return execute

    def test_loader_failure_keeps_the_task_log_and_original_exit_code(self):
        message = "bwrap: missing shared library libselinux.so.1"
        paths, bwrap, workspace = self.task_paths("loader", f"printf '{message}\\n' >&2\nexit 127\n")
        prefix = f"PATH={shlex.quote(str(bwrap.parent))}:$PATH; "
        environment = _ProcessBoundary(*_INSTALL_RESULTS[:2], self.shell(prefix))
        agent = self.agent(exec_backend="bwrap", bwrap=bwrap, workspace=str(workspace))
        with patch.multiple(threadmill_agent, **{name: PurePosixPath(path) for name, path in paths.items()}):
            with self.assertRaisesRegex(RuntimeError, "missing shared library libselinux"):
                asyncio.run(agent.install(environment))
        self.assertIsNone(environment.calls[-1]["user"])
        self.assertEqual(environment.last_result.return_code, 127)
        self.assertIn(message, environment.last_result.stdout)
        self.assertIn(message, (paths["_REMOTE_LOGS"] / "task-setup.txt").read_text())

    def test_cleanup_failure_cannot_hide_the_probe_result(self):
        fixtures = (
            ("primary", "cp() { printf 'copy denied\\n' >&2; return 73; }; "
             "rm() { printf 'cleanup denied\\n' >&2; return 74; }; ", 73),
            ("cleanup", "cp() { shift; command cp \"$@\"; }; "
             "rm() { if test \"$1\" = -rf; then printf 'cleanup denied\\n' >&2; return 74; "
             "fi; command rm \"$@\"; }; ", 1),
        )
        for name, prefix, expected_code in fixtures:
            with self.subTest(name=name):
                paths, bwrap, workspace = self.task_paths(name, "printf 'bubblewrap 0.8.0\\n'\n")
                task_prefix = f"PATH={shlex.quote(str(bwrap.parent))}:$PATH; " + prefix
                environment = _ProcessBoundary(*_INSTALL_RESULTS[:2], self.shell(task_prefix))
                agent = self.agent(exec_backend="bwrap", bwrap=bwrap, workspace=str(workspace))
                with patch.multiple(threadmill_agent, **{
                    key: PurePosixPath(path) for key, path in paths.items()
                }):
                    with self.assertRaisesRegex(RuntimeError, "cleanup denied"):
                        asyncio.run(agent.install(environment))
                self.assertEqual(environment.last_result.return_code, expected_code)
                self.assertIn("task_cleanup=no", environment.last_result.stdout)
                if name == "primary":
                    self.assertIn("copy denied", environment.last_result.stdout)
                else:
                    self.assertIn("task_directory_write=yes", environment.last_result.stdout)

    def test_task_path_shadowing_stops_before_doctor_or_model(self):
        for tool in ("bwrap", "strace"):
            with self.subTest(tool=tool):
                paths, bwrap, workspace = self.task_paths(tool + "-shadow", "exit 0\n")
                shadow = bwrap.parent / "shadow"
                shadow.mkdir()
                (shadow / tool).write_text("#!/bin/sh\nexit 0\n")
                (shadow / tool).chmod(0o755)
                prefix = (
                    f"PATH={shlex.quote(str(shadow))}:"
                    f"{shlex.quote(str(bwrap.parent))}:$PATH; "
                )
                environment = _ProcessBoundary(*_INSTALL_RESULTS[:2], self.shell(prefix))
                agent = self.agent(exec_backend="bwrap", bwrap=bwrap, workspace=str(workspace))
                with patch.multiple(threadmill_agent, **{
                    key: PurePosixPath(path) for key, path in paths.items()
                }):
                    with self.assertRaisesRegex(NonZeroAgentExitCodeError, tool + " PATH mismatch"):
                        asyncio.run(agent.install(environment))
                self.assertIsNone(environment.calls[-1]["user"])
                self.assertEqual(environment.last_result.return_code, 1)
                self.assertIn(tool + "_resolved=" + str(shadow / tool), environment.last_result.stdout)
                self.assertFalse(any("-exec-doctor" in call["command"] for call in environment.calls))
                self.assertFalse(any("console.log" in call["command"] for call in environment.calls))


if __name__ == "__main__":
    unittest.main()
