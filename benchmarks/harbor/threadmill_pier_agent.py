"""Pier entry point sharing Threadmill's installation, execution and logging."""

from pathlib import Path
from tempfile import TemporaryDirectory
from types import SimpleNamespace
from urllib.parse import urlsplit

from pier.agents.installed.base import BaseInstalledAgent
from pier.models.agent.network import NetworkAllowlist

from benchmarks.harbor.threadmill_agent import ThreadmillRunner


class Threadmill(ThreadmillRunner, BaseInstalledAgent):
    def __init__(self, *args, config=None, **kwargs):
        self.config_source = Path(config) if isinstance(config, str) else config
        super().__init__(*args, **kwargs)

    @property
    def model_connection(self):
        provider = (self.model_name or "openai").split("/", 1)[0].upper().replace("-", "_")
        return SimpleNamespace(
            base_url=self._get_env(f"{provider}_BASE_URL")
            or self._get_env("OPENAI_BASE_URL") or self._get_env("OPENAI_API_BASE"),
            api_key=self._get_env(f"{provider}_API_KEY") or self._get_env("OPENAI_API_KEY"),
        )

    def network_allowlist(self):
        domains = []
        for url in (self.model_connection.base_url, self._model_proxy):
            if url and urlsplit(url).hostname:
                domains.append(urlsplit(url).hostname)
        return NetworkAllowlist(domains=domains)

    def install_spec(self):
        # Installation uploads the locally compiled executable at setup time.
        return None

    async def _upload_config_text(self, environment, *, content, remote_path, filename):
        with TemporaryDirectory(prefix="threadmill-config-") as directory:
            path = Path(directory) / filename
            path.write_text(content)
            path.chmod(0o600)
            await environment.upload_file(path, remote_path)
