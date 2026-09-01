"""Harbor Agent adapter for a locally built Threadmill binary."""

from __future__ import annotations

import json
import os
import shlex
import tomllib
from pathlib import Path, PurePosixPath
from typing import Any, override

from harbor.agents.installed.base import BaseInstalledAgent, with_prompt_template
from harbor.agents.model_connection import ModelConnectionSpec
from harbor.environments.base import BaseEnvironment
from harbor.models.agent.context import AgentContext

_REMOTE_BINARY = PurePosixPath("/installed-agent/threadmill")
_REMOTE_TRACER = PurePosixPath("/usr/local/bin/strace")
_REMOTE_HOME = PurePosixPath("/tmp/threadmill-agent-home")
_REMOTE_TMP = PurePosixPath("/tmp/threadmill-agent-tmp")
_REMOTE_VFS = PurePosixPath("/threadmill-vfs")
_REMOTE_CONFIG = PurePosixPath("/tmp/threadmill-agent/config.yaml")
_REMOTE_USER_CONFIG = _REMOTE_HOME / ".threadmill" / "config.yaml"
_REMOTE_CREDENTIALS = _REMOTE_HOME / ".threadmill" / "credentials.yaml"
_REMOTE_LOGS = PurePosixPath("/logs/agent/threadmill")
_CREDENTIAL_NAME = "harbor"
_RUN_TIMEOUT_HEADROOM_SEC = 600
_DEFAULT_RUN_TIMEOUT_SEC = 144_000


def _yaml_string(value: str) -> str:
    return json.dumps(value, ensure_ascii=False)


def _runtime_config(
    base_url: str,
    model: str,
    context_window: int,
    exec_slots: int | None,
    model_proxy: str | None = None,
) -> str:
    lines = [
        "llm:",
        "  provider: openai-responses",
        f"  base_url: {_yaml_string(base_url)}",
        f"  credential: {_yaml_string(_CREDENTIAL_NAME)}",
        f"  model: {_yaml_string(model)}",
        f"  context_window: {context_window}",
        "exec:",
        "  external_sandbox: true",
        "  external_workspace_isolation: true",
    ]
    if model_proxy:
        lines.insert(3, f"  proxy_url: {_yaml_string(model_proxy)}")
    if exec_slots is not None:
        lines.append(f"  slots: {exec_slots}")
    lines.extend(
        [
            "vfs:",
            f"  live_root: {_yaml_string(_REMOTE_VFS.as_posix())}",
        ]
    )
    return "\n".join(lines) + "\n"


def _credentials(api_key: str) -> str:
    return f"{_CREDENTIAL_NAME}: {_yaml_string(api_key)}\n"


def _model_id(model_name: str | None) -> str:
    if not model_name:
        raise ValueError("Threadmill requires a Harbor model name")
    return model_name.split("/", 1)[-1]


def _last_runtime_snapshot(logs_dir: Path) -> dict[str, Any] | None:
    latest: dict[str, Any] | None = None
    state = logs_dir / "threadmill" / "state"
    for path in sorted(state.glob("*/threadmill.log")):
        for line in path.read_text(errors="replace").splitlines():
            try:
                event = json.loads(line)
            except json.JSONDecodeError:
                continue
            if event.get("msg") == "runtime snapshot":
                latest = event
    return latest


class Threadmill(BaseInstalledAgent):
    """Run Threadmill inside a Harbor task without changing its grader."""

    SUPPORTS_CONFIG = True
    MODEL_CONNECTION = ModelConnectionSpec(passthrough=True)

    def __init__(
        self,
        *args: Any,
        binary: str | os.PathLike[str] | None = None,
        tracer: str | os.PathLike[str] | None = None,
        context_window: int = 272_000,
        exec_slots: int | None = None,
        model_proxy: str | None = None,
        **kwargs: Any,
    ) -> None:
        candidate = binary or os.environ.get("THREADMILL_BINARY")
        if not candidate:
            raise ValueError(
                "Threadmill requires agent kwarg binary=/absolute/path/to/threadmill"
            )
        self._binary = Path(candidate).expanduser().resolve()
        if not self._binary.is_file():
            raise FileNotFoundError(f"Threadmill binary not found: {self._binary}")
        self._tracer = None if tracer is None else Path(tracer).expanduser().resolve()
        if self._tracer is not None and not self._tracer.is_file():
            raise FileNotFoundError(f"strace binary not found: {self._tracer}")
        self._context_window = int(context_window)
        if self._context_window <= 0:
            raise ValueError("context_window must be positive")
        self._exec_slots = None if exec_slots is None else int(exec_slots)
        if self._exec_slots is not None and self._exec_slots <= 0:
            raise ValueError("exec_slots must be positive")
        self._model_proxy = model_proxy
        super().__init__(*args, **kwargs)

    @staticmethod
    @override
    def name() -> str:
        return "threadmill"

    @override
    async def install(self, environment: BaseEnvironment) -> None:
        await environment.upload_file(self._binary, _REMOTE_BINARY.as_posix())
        if self._tracer is not None:
            await environment.upload_file(self._tracer, _REMOTE_TRACER.as_posix())
        tracer_chmod = (
            f"chmod 0755 {shlex.quote(_REMOTE_TRACER.as_posix())}; "
            if self._tracer is not None
            else ""
        )
        await self.exec_as_root(
            environment,
            command=(
                f"chmod 0755 {shlex.quote(_REMOTE_BINARY.as_posix())}; "
                f"{tracer_chmod}"
                f"mkdir -p {shlex.quote(_REMOTE_LOGS.as_posix())} "
                f"{shlex.quote(_REMOTE_VFS.as_posix())}; "
                f"probe=$(mktemp -d {_REMOTE_VFS.as_posix()}/probe.XXXXXX); "
                "trap 'umount \"$probe/merged\" >/dev/null 2>&1 || true; "
                "rm -rf \"$probe\"' EXIT; "
                "{ "
                "printf 'utc='; date -u +%FT%TZ; "
                "printf 'identity='; id; "
                "printf 'kernel='; uname -srmo; "
                "printf 'cpus='; nproc; "
                "printf 'strace='; command -v strace || printf 'unavailable\\n'; "
                "printf 'fuse_overlayfs='; command -v fuse-overlayfs || printf 'unavailable\\n'; "
                "printf 'fusermount3='; command -v fusermount3 || printf 'unavailable\\n'; "
                "printf 'devices='; stat -c '/workspace/repo:%d /tmp:%d' "
                "/workspace/repo /tmp; "
                f"stat -c '{_REMOTE_VFS.as_posix()}:%d' {_REMOTE_VFS.as_posix()}; "
                "printf 'filesystems\\n'; df -T /workspace/repo /tmp; "
                f"df -T {_REMOTE_VFS.as_posix()}; "
                "printf 'cgroup_cpu='; cat /sys/fs/cgroup/cpu.max 2>/dev/null || printf 'unknown\\n'; "
                "printf 'cgroup_memory='; cat /sys/fs/cgroup/memory.max 2>/dev/null || printf 'unknown\\n'; "
                "printf 'cap_eff='; awk '/^CapEff:/ {print $2}' /proc/self/status; "
                "mkdir -p \"$probe/lower\" \"$probe/upper\" \"$probe/work\" \"$probe/merged\"; "
                "printf x >\"$probe/lower/file\"; "
                "if mount -t overlay overlay "
                "-o lowerdir=\"$probe/lower\",upperdir=\"$probe/upper\",workdir=\"$probe/work\" "
                "\"$probe/merged\" 2>/dev/null; then "
                "printf 'native_overlay=yes\\n'; umount \"$probe/merged\"; "
                "else printf 'native_overlay=no\\n'; fi; "
                "printf base >\"$probe/merged/file\"; "
                "if command -v unshare >/dev/null 2>&1 && "
                "unshare --mount --pid --fork --propagation unchanged "
                "sh -c 'mount --make-rprivate / && "
                "mount --bind \"$1\" \"$2\" && mount -t proc proc /proc && "
                "test \"$(cat \"$2/file\")\" = x' _ "
                "\"$probe/lower\" \"$probe/merged\" 2>/dev/null; then "
                "printf 'mount_namespace=yes\\n'; "
                "else printf 'mount_namespace=no\\n'; fi; "
                "if cp --reflink=always /workspace/repo/README.markdown "
                "\"$probe/reflink\" 2>/dev/null; then printf 'repo_to_vfs_reflink=yes\\n'; "
                "else printf 'repo_to_vfs_reflink=no\\n'; fi; "
                f"}} >{shlex.quote((_REMOTE_LOGS / 'setup.txt').as_posix())} 2>&1; "
                f"grep -qx 'mount_namespace=yes' "
                f"{shlex.quote((_REMOTE_LOGS / 'setup.txt').as_posix())}"
            ),
        )

    async def _write_configuration(
        self,
        environment: BaseEnvironment,
        base_url: str,
        model: str,
        api_key: str,
    ) -> None:
        await self.exec_as_agent(
            environment,
            command=(
                f"mkdir -p {shlex.quote(_REMOTE_CONFIG.parent.as_posix())} "
                f"{shlex.quote(_REMOTE_CREDENTIALS.parent.as_posix())} "
                f"{shlex.quote(_REMOTE_TMP.as_posix())} "
                f"{shlex.quote(_REMOTE_LOGS.as_posix())} && "
                f"chmod 0700 {shlex.quote(_REMOTE_CREDENTIALS.parent.as_posix())} "
                f"{shlex.quote(_REMOTE_TMP.as_posix())}"
            ),
        )
        if self.config_source is not None:
            if isinstance(self.config_source, Path):
                content = self.config_source.read_text()
            else:
                content = json.dumps(self.config_source, ensure_ascii=False, indent=2)
            await self._upload_config_text(
                environment,
                content=content,
                remote_path=_REMOTE_USER_CONFIG.as_posix(),
                filename="config.yaml",
            )
        await self._upload_config_text(
            environment,
            content=_runtime_config(
                base_url,
                model,
                self._context_window,
                self._exec_slots,
                self._model_proxy,
            ),
            remote_path=_REMOTE_CONFIG.as_posix(),
            filename="config.yaml",
        )
        await self._upload_config_text(
            environment,
            content=_credentials(api_key),
            remote_path=_REMOTE_CREDENTIALS.as_posix(),
            filename="credentials.yaml",
        )
        await self.exec_as_agent(
            environment,
            command=f"chmod 0600 {shlex.quote(_REMOTE_CREDENTIALS.as_posix())}",
        )

    def _run_timeout_sec(self) -> int:
        try:
            trial = Path(self.logs_dir).parent
            config = json.loads((trial / "config.json").read_text(encoding="utf-8"))
            task_path = Path(str((config.get("task") or {}).get("path") or ""))
            if task_path.parts:
                task = tomllib.loads(
                    (task_path / "task.toml").resolve().read_text(encoding="utf-8")
                )
                wall = float((task.get("agent") or {}).get("timeout_sec") or 0)
                if wall > 0:
                    return int(wall) + _RUN_TIMEOUT_HEADROOM_SEC
        except (OSError, ValueError, TypeError, json.JSONDecodeError, tomllib.TOMLDecodeError):
            pass
        return _DEFAULT_RUN_TIMEOUT_SEC

    @override
    @with_prompt_template
    async def run(
        self,
        instruction: str,
        environment: BaseEnvironment,
        context: AgentContext,
    ) -> None:
        access = self.model_connection
        if not access.base_url:
            raise ValueError("Threadmill requires an OpenAI-compatible base URL")
        if not access.api_key:
            raise ValueError("Threadmill requires an API key")
        model = _model_id(self.model_name)
        await self._write_configuration(
            environment,
            access.base_url,
            model,
            access.api_key,
        )

        home = shlex.quote(_REMOTE_HOME.as_posix())
        logs = shlex.quote(_REMOTE_LOGS.as_posix())
        command = (
            "set +e; "
            f"{shlex.quote(_REMOTE_BINARY.as_posix())} "
            "-C /workspace/repo "
            f"-config {shlex.quote(_REMOTE_CONFIG.as_posix())} "
            f"-p {shlex.quote(instruction)} "
            f"2>&1 | tee {logs}/console.log; "
            "pipeline=(\"${PIPESTATUS[@]}\"); "
            "status=${pipeline[0]}; "
            "if [ \"$status\" -eq 0 ] && [ \"${pipeline[1]}\" -ne 0 ]; then "
            "status=${pipeline[1]}; fi; "
            "collect_status=0; "
            f"state_root={home}/.threadmill/projects; "
            f"mkdir -p {logs}/state || collect_status=$?; "
            "for project in \"$state_root\"/*; do "
            "[ -d \"$project\" ] || continue; "
            "name=$(basename \"$project\"); "
            f"dest={logs}/state/\"$name\"; "
            "mkdir -p \"$dest\" || collect_status=$?; "
            "for item in graphs checkpoints progress; do "
            "if [ -e \"$project/$item\" ]; then "
            "cp -a \"$project/$item\" \"$dest/\" || collect_status=$?; fi; "
            "done; "
            "if [ -f \"$project/threadmill.log\" ]; then "
            "cp -a \"$project/threadmill.log\" \"$dest/\" || collect_status=$?; fi; "
            "done; "
            f"if [ -d {shlex.quote(_REMOTE_VFS.as_posix())} ]; then "
            f"tar -C {shlex.quote(_REMOTE_VFS.as_posix())} "
            "--exclude='./.threadmill-exec-*' --exclude='./probe.*' "
            "--exclude='./.tmp-*' --exclude='./.overlay-tmp-*' "
            # .floor is a full clone of the repo and .replaced holds displaced
            # publication content; neither is state worth shipping per trial.
            "--exclude='./.floor' --exclude='./.replaced' "
            f"-cpf {logs}/vfs-state.tar . || collect_status=$?; fi; "
            "if [ \"$status\" -eq 0 ] && [ \"$collect_status\" -ne 0 ]; then "
            "status=$collect_status; fi; "
            "exit \"$status\""
        )
        await self.exec_as_agent(
            environment,
            command=command,
            env={
                "HOME": _REMOTE_HOME.as_posix(),
                "TMPDIR": _REMOTE_TMP.as_posix(),
            },
            cwd="/workspace/repo",
            timeout_sec=self._run_timeout_sec(),
        )

    @override
    def populate_context_post_run(self, context: AgentContext) -> None:
        snapshot = _last_runtime_snapshot(self.logs_dir)
        if snapshot is None:
            return
        context.n_input_tokens = int(snapshot.get("input_tokens", 0)) + int(
            snapshot.get("memory_input_tokens", 0)
        )
        context.n_cache_tokens = int(snapshot.get("cached_tokens", 0)) + int(
            snapshot.get("memory_cached_tokens", 0)
        )
        context.n_output_tokens = int(snapshot.get("tokens", 0)) + int(
            snapshot.get("memory_ops_tokens", 0)
        )
        context.metadata = {"threadmill_runtime_snapshot": snapshot}
