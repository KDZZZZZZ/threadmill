import asyncio
import tempfile
import unittest
from pathlib import Path

from pier.agents.installed.base import BaseInstalledAgent
from pier.models.agent.context import AgentContext

from benchmarks.harbor.threadmill_pier_agent import Threadmill


class ThreadmillPierTest(unittest.TestCase):
    def test_pier_runs_shared_adapter_with_its_own_credentials_and_config(self):
        class CapturingThreadmill(Threadmill):
            async def exec_as_agent(self, environment, command, **kwargs):
                self.calls.append((command, kwargs))

        class Environment:
            async def upload_file(self, source, destination):
                uploads[destination] = Path(source).read_text()

        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            binary = root / "binary"
            binary.touch()
            config = root / "custom.yaml"
            config.write_text("exec:\n  slots: 2\n")
            agent = CapturingThreadmill(
                root, binary=binary, workspace="/app", config=str(config),
                model_name="openai/model", version="test",
                extra_env={"OPENAI_API_KEY": "test-key", "OPENAI_BASE_URL": "https://model.example/v1"},
            )
            agent.calls = []
            uploads = {}
            self.assertIsInstance(agent, BaseInstalledAgent)
            self.assertEqual(agent.version(), "test")
            self.assertEqual(agent.network_allowlist().domains, ["model.example"])
            asyncio.run(agent.run("fix the task", Environment(), AgentContext()))
            command, options = agent.calls[-1]
            self.assertEqual(options["cwd"], "/app")
            self.assertIn("-C /app ", command)
            self.assertEqual(uploads["/tmp/threadmill-agent-home/.threadmill/config.yaml"], config.read_text())
            self.assertIn("test-key", uploads["/tmp/threadmill-agent-home/.threadmill/credentials.yaml"])

class ThreadmillPierEnvironmentTest(unittest.TestCase):
    def test_offline_task_keeps_filtered_egress_with_vfs_mounts(self):
        from pier.models.agent.network import NetworkAllowlist
        from pier.models.task.config import EnvironmentConfig
        from pier.models.trial.paths import TrialPaths
        from benchmarks.harbor.threadmill_pier_environment import ThreadmillDockerEnvironment

        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            environment = root / "environment"
            environment.mkdir()
            (environment / "Dockerfile").write_text("FROM python:3.11-slim\n")
            trial = root / "trial"
            trial.mkdir()
            sandbox = ThreadmillDockerEnvironment(
                environment_dir=environment, environment_name="threadmill-test",
                session_id="threadmill-test", trial_paths=TrialPaths(trial_dir=trial),
                task_env_config=EnvironmentConfig(allow_internet=False),
                network_allowlist=NetworkAllowlist(domains=["model.example"]),
            )
            sandbox._prepare_egress_proxy_compose()
            paths = sandbox._docker_compose_paths
            self.assertIn(sandbox._egress_proxy_compose_path, paths)
            self.assertTrue(any(p.name == "docker-compose-workspace-isolation.yaml" for p in paths))
            self.assertTrue(sandbox._egress_proxy_env)
            self.assertFalse(sandbox.task_env_config.allow_internet)


if __name__ == "__main__":
    unittest.main()
