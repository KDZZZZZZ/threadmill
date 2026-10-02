from __future__ import annotations

import asyncio
import json
import struct
import subprocess
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch
import yaml

from harbor.agents.model_connection import ResolvedModelConnection
from harbor.models.agent.context import AgentContext

from benchmarks.harbor.threadmill_agent import (
    Threadmill,
    _credentials,
    _model_id,
    _runtime_config,
)


class ThreadmillAgentTest(unittest.TestCase):
    def test_doctor_prepares_install_directories_as_root_and_keeps_image_user(self):
        from benchmarks.harbor.doctor import check

        prepared, calls = False, []
        setup = "mount_namespace=yes\nrepo_to_vfs_reflink=yes\nstrace_version=6.8\nstrace_probe=yes\n"

        def process(command, **kwargs):
            nonlocal prepared
            command = list(command)
            calls.append(command)
            stdout = ""
            if command[:2] == ["docker", "info"]:
                stdout = json.dumps({"Driver": "btrfs", "DockerRootDir": "/dedicated/data"})
            elif command[:3] == ["docker", "image", "inspect"]:
                stdout = json.dumps([{"Id": "sha256:task", "Config": {"User": "agent"}}])
            elif command[0] == "findmnt":
                stdout = "btrfs"
            elif command[-1] == "-V":
                stdout = "strace -- version 6.8"
            elif command[:2] == ["docker", "exec"]:
                if "--user" in command and "mkdir -p" in command[-1] and command[command.index("--user") + 1] == "root":
                    prepared = True
                stdout = setup
                if "-exec-doctor" in command:
                    self.assertNotIn("--user", command)
                    stdout = '{"exec_dependency_tracing":true,"exec_dependency_tracing_enabled":true}'
            elif command[:2] == ["docker", "cp"] and "/installed-agent/" in command[-1] and not prepared:
                return subprocess.CompletedProcess(command, 1, stdout="", stderr="install directory missing")
            return subprocess.CompletedProcess(command, 0, stdout=stdout, stderr="")

        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            binary, tracer = root / "threadmill", root / "strace"
            binary.touch()
            elf = bytearray(120)
            elf[:16] = b"\x7fELF\x02\x01\x01" + b"\x00" * 9
            struct.pack_into("<HHIQQQIHHHHHH", elf, 16,
                             2, 62, 1, 0, 64, 0, 0, 64, 56, 1, 0, 0, 0)
            struct.pack_into("<IIQQQQQQ", elf, 64, 1, 5, 0, 0, 0, len(elf), len(elf), 4096)
            tracer.write_bytes(elf)
            with patch("benchmarks.harbor.doctor.subprocess.run", side_effect=process):
                result = check(binary=binary, tracer=tracer, image="task/image",
                               workspace="/workspace/repo", runtime="test-harbor")
            self.assertTrue(result["ok"], result["failures"])
            self.assertNotIn("--user", next(call for call in calls if call[:2] == ["docker", "create"]))

    def test_cache_modes_follow_existing_exec_cache_schema(self) -> None:
        for mode, enabled, rate in (("off", False, .01), ("shadow", True, 1.0), ("live", True, .01)):
            config = yaml.safe_load(_runtime_config("https://example.test/v1", "model", 128000,
                                                    None, cache_mode=mode))
            self.assertEqual(config["exec"]["cache"], {"enabled": enabled, "verify_sample_rate": rate})
            self.assertNotIn("cache", config)

    def test_install_rejects_dynamic_tracer_without_upload(self) -> None:
        class Environment:
            async def upload_file(self, source, destination):
                self.uploaded = True

        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            binary = root / "threadmill"
            binary.touch()
            environment = Environment()
            environment.uploaded = False
            agent = Threadmill(root, binary=binary, tracer="/bin/sh",
                               model_name="openai/model")
            with self.assertRaisesRegex(ValueError, "static"):
                asyncio.run(agent.install(environment))
            self.assertFalse(environment.uploaded)

    def test_install_rejects_failed_reflink_before_run(self) -> None:
        class Environment:
            async def upload_file(self, source, destination):
                return None

        class ProbeThreadmill(Threadmill):
            setup = "mount_namespace=yes\nrepo_to_vfs_reflink=no\nstrace_version=6.8\nstrace_probe=yes\n"

            async def exec_as_root(self, environment, command, **kwargs):
                return SimpleNamespace(stdout=self.setup)

        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            binary = root / "threadmill"
            binary.touch()
            tracer = root / "strace"
            elf = bytearray(120)
            elf[:16] = b"\x7fELF\x02\x01\x01" + b"\x00" * 9
            struct.pack_into("<HHIQQQIHHHHHH", elf, 16,
                             2, 62, 1, 0, 64, 0, 0, 64, 56, 1, 0, 0, 0)
            struct.pack_into("<IIQQQQQQ", elf, 64,
                             1, 5, 0, 0, 0, len(elf), len(elf), 4096)
            tracer.write_bytes(elf)
            agent = ProbeThreadmill(root, binary=binary, tracer=tracer,
                                    model_name="openai/model")

            with self.assertRaisesRegex(RuntimeError, "repo_to_vfs_reflink"):
                asyncio.run(agent.install(Environment()))

            agent.setup = "mount_namespace=yes\nrepo_to_vfs_reflink=yes\nstrace_version=5.2\nstrace_probe=yes\n"
            with self.assertRaisesRegex(RuntimeError, "strace.*5.3"):
                asyncio.run(agent.install(Environment()))

    def test_run_rejects_unavailable_tracing_before_model_command(self) -> None:
        class ProbeThreadmill(Threadmill):
            @property
            def model_connection(self) -> ResolvedModelConnection:
                return ResolvedModelConnection(api_key="test-key", base_url="https://example.test/v1")

            async def _write_configuration(self, *args, **kwargs):
                return None

            async def exec_as_agent(self, environment, command, **kwargs):
                self.calls.append(command)
                return SimpleNamespace(stdout='{"exec_dependency_tracing": false}')

        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            binary = root / "threadmill"
            binary.touch()
            agent = ProbeThreadmill(root, binary=binary, model_name="openai/model")
            agent.calls = []
            with self.assertRaisesRegex(RuntimeError, "exec_dependency_tracing"):
                asyncio.run(agent.run("do it", object(), AgentContext()))
            self.assertEqual(len(agent.calls), 1)
            self.assertIn("-exec-doctor", agent.calls[0])
            self.assertNotIn("-p ", agent.calls[0])

    def test_runtime_config_uses_external_harbor_boundary(self) -> None:
        config = _runtime_config(
            "https://example.test/v1",
            'model:"quoted"',
            200_000,
            32,
        )

        self.assertIn('base_url: "https://example.test/v1"', config)
        self.assertIn('model: "model:\\"quoted\\\""', config)
        self.assertIn("context_window: 200000", config)
        self.assertIn("external_sandbox: true", config)
        self.assertIn("external_workspace_isolation: true", config)
        self.assertIn('live_root: "/threadmill-vfs"', config)
        self.assertIn(
            "exec:\n"
            "  external_sandbox: true\n"
            "  external_workspace_isolation: true\n"
            "  require_dependency_tracing: true\n"
            "  slots: 32\n"
            "vfs:\n",
            config,
        )

    def test_credentials_are_separate_from_runtime_config(self) -> None:
        secret = "sk-test-value"

        self.assertNotIn(
            secret,
            _runtime_config("https://example.test/v1", "model", 128_000, None),
        )
        self.assertEqual(_credentials(secret), 'harbor: "sk-test-value"\n')

    def test_runtime_config_routes_only_model_requests_through_proxy(self) -> None:
        config = _runtime_config(
            "https://example.test/v1",
            "model",
            128_000,
            None,
            "http://172.17.0.1:7890",
        )

        self.assertIn('proxy_url: "http://172.17.0.1:7890"', config)
        self.assertNotIn("HTTP_PROXY", config)
        self.assertNotIn("HTTPS_PROXY", config)

    def test_model_id_removes_only_harbor_provider_prefix(self) -> None:
        self.assertEqual(_model_id("openai/gpt-5.6-luna"), "gpt-5.6-luna")
        self.assertEqual(_model_id("local-model"), "local-model")

    def test_optional_tracer_must_be_a_file(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            binary = root / "threadmill"
            binary.write_bytes(b"binary")
            tracer = root / "strace"
            tracer.write_bytes(b"tracer")

            agent = Threadmill(
                root,
                model_name="deepseek/model",
                binary=binary,
                tracer=tracer,
            )
            self.assertEqual(agent._tracer, tracer.resolve())

            with self.assertRaises(FileNotFoundError):
                Threadmill(
                    root,
                    model_name="deepseek/model",
                    binary=binary,
                    tracer=root / "missing",
                )

    def test_context_uses_last_runtime_snapshot(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            logs = Path(temp)
            state = logs / "threadmill" / "state" / "project"
            state.mkdir(parents=True)
            snapshots = [
                {"msg": "runtime snapshot", "input_tokens": 1},
                {
                    "msg": "runtime snapshot",
                    "input_tokens": 100,
                    "memory_input_tokens": 20,
                    "cached_tokens": 40,
                    "memory_cached_tokens": 5,
                    "tokens": 30,
                    "memory_ops_tokens": 7,
                },
            ]
            (state / "threadmill.log").write_text(
                "\n".join(json.dumps(item) for item in snapshots) + "\n"
            )
            binary = logs / "threadmill-bin"
            binary.write_bytes(b"binary")
            agent = Threadmill(
                logs,
                model_name="openai/gpt-5.6-luna",
                binary=binary,
            )
            context = AgentContext()

            agent.populate_context_post_run(context)

            self.assertEqual(context.n_input_tokens, 120)
            self.assertEqual(context.n_cache_tokens, 45)
            self.assertEqual(context.n_output_tokens, 37)
            self.assertEqual(
                context.metadata["threadmill_runtime_snapshot"]["input_tokens"],
                100,
            )

    def test_run_uses_task_wall_and_collects_recovery_state(self) -> None:
        class CapturingThreadmill(Threadmill):
            def __init__(self, *args, **kwargs) -> None:
                self.calls: list[dict[str, object]] = []
                super().__init__(*args, **kwargs)

            @property
            def model_connection(self) -> ResolvedModelConnection:
                return ResolvedModelConnection(
                    api_key="test-key",
                    base_url="https://example.test/v1",
                )

            async def _write_configuration(self, *args, **kwargs) -> None:
                return None

            async def exec_as_agent(self, environment, command, **kwargs):
                self.calls.append({"command": command, **kwargs})
                return SimpleNamespace(stdout=json.dumps({
                    "exec_dependency_tracing": True,
                    "exec_dependency_tracing_enabled": True,
                }))

        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            trial = root / "trial"
            logs = trial / "agent"
            task = root / "task"
            logs.mkdir(parents=True)
            task.mkdir()
            (task / "task.toml").write_text(
                "[agent]\ntimeout_sec = 21600.0\n",
                encoding="utf-8",
            )
            (trial / "config.json").write_text(
                json.dumps({"task": {"path": str(task)}}),
                encoding="utf-8",
            )
            binary = root / "threadmill"
            binary.write_bytes(b"binary")
            agent = CapturingThreadmill(
                logs,
                model_name="deepseek/deepseek-v4-flash",
                binary=binary,
                workspace="/app",
            )

            asyncio.run(agent.run("do it", object(), AgentContext()))

            self.assertEqual(len(agent.calls), 2)
            self.assertIn("-exec-doctor", str(agent.calls[0]["command"]))
            call = agent.calls[1]
            self.assertEqual(call.get("timeout_sec"), 22200)
            self.assertEqual(call.get("cwd"), "/app")
            self.assertIn("-C /app ", str(call["command"]))
            command = str(call["command"])
            self.assertIn("vfs-state.tar", command)
            self.assertIn(".threadmill-exec-*", command)

    def test_write_configuration_secures_uploaded_credentials(self) -> None:
        class CapturingThreadmill(Threadmill):
            def __init__(self, *args, **kwargs) -> None:
                self.commands: list[str] = []
                self.uploads: list[str] = []
                super().__init__(*args, **kwargs)

            async def exec_as_agent(self, environment, command, **kwargs):
                self.commands.append(str(command))

            async def _upload_config_text(
                self, environment, *, content, remote_path, filename
            ) -> None:
                self.uploads.append(str(remote_path))

        with tempfile.TemporaryDirectory() as temp:
            binary = Path(temp) / "threadmill"
            binary.write_bytes(b"binary")
            agent = CapturingThreadmill(
                Path(temp),
                model_name="deepseek/deepseek-v4-flash",
                binary=binary,
            )

            asyncio.run(
                agent._write_configuration(
                    object(),
                    "https://example.test/v1",
                    "model",
                    "test-key",
                )
            )

            self.assertTrue(agent.uploads[-1].endswith("credentials.yaml"))
            self.assertIn(
                "chmod 0600 /tmp/threadmill-agent-home/.threadmill/credentials.yaml",
                agent.commands[-1],
            )


if __name__ == "__main__":
    unittest.main()
