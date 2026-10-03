import asyncio
import struct
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace

from pier.agents.installed.base import BaseInstalledAgent
from pier.models.agent.context import AgentContext

from benchmarks.harbor.threadmill_pier_agent import Threadmill


class ThreadmillPierTest(unittest.TestCase):
    def test_pier_runs_shared_adapter_with_its_own_credentials_and_config(self):
        class Environment:
            default_user = None

            async def upload_file(self, source, destination):
                uploads[destination] = Path(source).read_bytes()

            def agent_process_env(self, env):
                return env

            async def exec(self, command, user=None, **kwargs):
                calls.append((command, {"user": user, **kwargs}))
                stdout = ""
                if "id -u; id -g" in command:
                    stdout = "2000\n2000\n"
                elif "task-setup.txt" in command:
                    stdout = "task_repo_to_vfs_reflink=yes\ntask_directory_write=yes\n"
                elif "setup.txt" in command:
                    stdout = ("mount_namespace=yes\nrepo_to_vfs_reflink=yes\n"
                              "strace_version=6.8\nstrace_probe=yes\n")
                elif "-exec-doctor" in command:
                    stdout = '{"exec_dependency_tracing":true,"exec_dependency_tracing_enabled":true}'
                return SimpleNamespace(stdout=stdout, stderr="", return_code=0)

        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            binary = root / "binary"
            binary.touch()
            tracer = root / "strace"
            elf = bytearray(120)
            elf[:16] = b"\x7fELF\x02\x01\x01" + b"\x00" * 9
            struct.pack_into("<HHIQQQIHHHHHH", elf, 16, 2, 62, 1, 0, 64, 0, 0, 64, 56, 1, 0, 0, 0)
            struct.pack_into("<IIQQQQQQ", elf, 64, 1, 5, 0, 0, 0, len(elf), len(elf), 4096)
            tracer.write_bytes(elf)
            config = root / "custom.yaml"
            config.write_text("exec:\n  slots: 2\n")
            agent = Threadmill(
                root, binary=binary, tracer=tracer, workspace="/app", config=str(config),
                model_name="openai/model", version="test",
                extra_env={"OPENAI_API_KEY": "test-key", "OPENAI_BASE_URL": "https://model.example/v1"},
            )
            calls = []
            uploads = {}
            self.assertIsInstance(agent, BaseInstalledAgent)
            self.assertEqual(agent.version(), "test")
            self.assertEqual(agent.network_allowlist().domains, ["model.example"])
            environment = Environment()
            asyncio.run(agent.install(environment))
            asyncio.run(agent.run("fix the task", environment, AgentContext()))
            self.assertTrue(any("-exec-doctor" in call[0] for call in calls))
            command, options = calls[-1]
            self.assertEqual(options["cwd"], "/app")
            self.assertIsNone(options["user"])
            self.assertIsNone(calls[0][1]["user"])
            self.assertIn("-C /app ", command)
            self.assertEqual(uploads["/tmp/threadmill-agent-home/.threadmill/config.yaml"], config.read_bytes())
            self.assertIn(b"test-key", uploads["/tmp/threadmill-agent-home/.threadmill/credentials.yaml"])
            permissions = next(call for call in calls if "chown 2000:2000" in call[0]
                               and "credentials.yaml" in call[0])
            self.assertEqual(permissions[1]["user"], "root")

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
