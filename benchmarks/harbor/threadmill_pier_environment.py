"""Add workspace mounts while retaining Pier's filtered model egress."""

import os
from pathlib import Path

from pier.environments.docker.docker import DockerEnvironment


class ThreadmillDockerEnvironment(DockerEnvironment):
    @property
    def _docker_compose_paths(self):
        paths = super()._docker_compose_paths
        directory = Path(__file__).resolve().parent
        paths.append(directory / "docker-compose-workspace-isolation.yaml")
        if os.environ.get("THREADMILL_BUILD_PROXY"):
            paths.append(directory / "docker-compose-build-proxy.yaml")
        return paths
