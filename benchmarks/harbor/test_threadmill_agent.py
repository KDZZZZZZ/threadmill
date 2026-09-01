from __future__ import annotations

import asyncio
import json
import tempfile
import unittest
from pathlib import Path

from harbor.agents.model_connection import ResolvedModelConnection
from harbor.models.agent.context import AgentContext

from benchmarks.harbor.threadmill_agent import (
    Threadmill,
    _credentials,
    _model_id,
    _runtime_config,
)


class ThreadmillAgentTest(unittest.TestCase):
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
            )

            asyncio.run(agent.run("do it", object(), AgentContext()))

            self.assertEqual(len(agent.calls), 1)
            call = agent.calls[0]
            self.assertEqual(call.get("timeout_sec"), 22200)
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
