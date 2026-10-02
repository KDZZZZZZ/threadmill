"""Harbor Agent adapter for a locally built Threadmill binary."""

from __future__ import annotations

import json
import os
import re
import shlex
import struct
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


def _require_static_tracer(path: Path | None) -> None:
    if path is None:
        raise ValueError("THREADMILL_STRACE_BINARY must point to a static strace >= 5.3")
    data = path.read_bytes()
    try:
        if data[:4] != b"\x7fELF" or data[4] not in (1, 2) or data[5] not in (1, 2):
            raise ValueError("not ELF")
        endian = "<" if data[5] == 1 else ">"
        wide = data[4] == 2
        word = "Q" if wide else "I"
        phoff = struct.unpack_from(endian + word, data, 32 if wide else 28)[0]
        size, count = struct.unpack_from(endian + "HH", data, 54 if wide else 42)
        if not count or size < (56 if wide else 32) or phoff + size * count > len(data):
            raise ValueError("invalid program headers")
        for index in range(count):
            offset = phoff + size * index
            kind = struct.unpack_from(endian + "I", data, offset)[0]
            if kind == 3:  # PT_INTERP requires a container dynamic loader.
                raise ValueError("dynamic interpreter")
            if kind == 2:  # Static PIE is allowed only without DT_NEEDED libraries.
                start = struct.unpack_from(endian + word, data, offset + (8 if wide else 4))[0]
                length = struct.unpack_from(endian + word, data, offset + (32 if wide else 16))[0]
                if start + length > len(data):
                    raise ValueError("invalid dynamic section")
                for entry in range(start, start + length, 16 if wide else 8):
                    tag = struct.unpack_from(endian + word, data, entry)[0]
                    if tag == 0:
                        break
                    if tag == 1:
                        raise ValueError("dynamic library")
    except (ValueError, IndexError, struct.error) as error:
        raise ValueError(f"strace must be a static ELF executable: {path}") from error


def _yaml_string(value: str) -> str:
    return json.dumps(value, ensure_ascii=False)


def _runtime_config(
    base_url: str,
    model: str,
    context_window: int,
    exec_slots: int | None,
    model_proxy: str | None = None,
    cache_mode: str | None = None,
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
        "  require_dependency_tracing: true",
    ]
    if model_proxy:
        lines.insert(3, f"  proxy_url: {_yaml_string(model_proxy)}")
    if exec_slots is not None:
        lines.append(f"  slots: {exec_slots}")
    if cache_mode:
        lines.extend([
            "  cache:",
            f"    enabled: {'false' if cache_mode == 'off' else 'true'}",
            f"    verify_sample_rate: {1.0 if cache_mode == 'shadow' else 0.01}",
        ])
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


class ThreadmillRunner:
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
        workspace: str = "/workspace/repo",
        cache_mode: str | None = None,
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
        tracer = tracer or os.environ.get("THREADMILL_STRACE_BINARY")
        self._tracer = None if tracer is None else Path(tracer).expanduser().resolve()
        if self._tracer is not None and not self._tracer.is_file():
            raise FileNotFoundError(f"strace binary not found: {self._tracer}")
        self._context_window = int(context_window)
        if self._context_window <= 0:
            raise ValueError("context_window must be positive")
        self._exec_slots = None if exec_slots is None else int(exec_slots)
        if self._exec_slots is not None and self._exec_slots <= 0:
            raise ValueError("exec_slots must be positive")
        self._workspace = PurePosixPath(workspace)
        if not self._workspace.is_absolute():
            raise ValueError("workspace must be an absolute container path")
        self._model_proxy = model_proxy
        if cache_mode not in (None, "off", "shadow", "live"):
            raise ValueError("cache_mode must be off, shadow, or live")
        self._cache_mode = cache_mode
        super().__init__(*args, **kwargs)

    @staticmethod
    @override
    def name() -> str:
        return "threadmill"

    @override
    async def install(self, environment: BaseEnvironment) -> None:
        _require_static_tracer(self._tracer)
        await environment.upload_file(self._binary, _REMOTE_BINARY.as_posix())
        if self._tracer is not None:
            await environment.upload_file(self._tracer, _REMOTE_TRACER.as_posix())
        workspace = shlex.quote(self._workspace.as_posix())
        tracer_chmod = (
            f"chmod 0755 {shlex.quote(_REMOTE_TRACER.as_posix())}; "
            if self._tracer is not None
            else ""
        )
        result = await self.exec_as_root(
            environment,
            command=(
                "set -eu; "
                f"chmod 0755 {shlex.quote(_REMOTE_BINARY.as_posix())}; "
                f"{tracer_chmod}"
                f"mkdir -p {shlex.quote(_REMOTE_LOGS.as_posix())} "
                f"{shlex.quote(_REMOTE_VFS.as_posix())}; "
                f"probe=$(mktemp -d {_REMOTE_VFS.as_posix()}/probe.XXXXXX); "
                f"repo_probe=$(mktemp {workspace}/.threadmill-reflink-probe.XXXXXX); "
                "trap 'umount \"$probe/merged\" >/dev/null 2>&1 || true; "
                "rm -rf \"$probe\"; rm -f \"$repo_probe\"' EXIT; "
                "{ "
                "printf 'utc='; date -u +%FT%TZ; "
                "printf 'identity='; id; "
                "printf 'kernel='; uname -srmo; "
                "printf 'cpus='; nproc; "
                "printf 'strace='; command -v strace || printf 'unavailable\\n'; "
                "printf 'fuse_overlayfs='; command -v fuse-overlayfs || printf 'unavailable\\n'; "
                "printf 'fusermount3='; command -v fusermount3 || printf 'unavailable\\n'; "
                "printf 'devices='; stat -c '%n:%d' "
                f"{workspace} /tmp; "
                f"stat -c '{_REMOTE_VFS.as_posix()}:%d' {_REMOTE_VFS.as_posix()}; "
                f"printf 'filesystems\\n'; df -T {workspace} /tmp; "
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
                "dd if=/dev/urandom of=\"$repo_probe\" bs=4096 count=1 status=none; "
                "if test -s \"$repo_probe\" && cp --reflink=always \"$repo_probe\" "
                "\"$probe/reflink\" 2>/dev/null && cmp \"$repo_probe\" \"$probe/reflink\"; "
                "then printf 'repo_to_vfs_reflink=yes\\n'; "
                "else printf 'repo_to_vfs_reflink=no\\n'; fi; "
                "printf 'strace_version='; "
                f"{_REMOTE_TRACER.as_posix()} -V 2>/dev/null | "
                "sed -n '1s/^.* version //p'; "
                f"if {_REMOTE_TRACER.as_posix()} -qq -f -yy -e trace=%file "
                "-o \"$probe/strace.log\" sh -c 'cat \"$1\" >/dev/null' _ \"$repo_probe\" "
                "2>/dev/null && grep -Fq \"$repo_probe\" \"$probe/strace.log\"; "
                "then printf 'strace_probe=yes\\n'; else printf 'strace_probe=no\\n'; fi; "
                f"}} >{shlex.quote((_REMOTE_LOGS / 'setup.txt').as_posix())} 2>&1; "
                f"cat {shlex.quote((_REMOTE_LOGS / 'setup.txt').as_posix())}"
            ),
        )
        setup = (result.stdout or "").splitlines()
        for requirement in ("mount_namespace", "repo_to_vfs_reflink", "strace_probe"):
            if f"{requirement}=yes" not in setup:
                raise RuntimeError(f"Threadmill preflight failed: {requirement}")
        version = next((line for line in setup if line.startswith("strace_version=")), "")
        match = re.fullmatch(r"strace_version=(\d+)\.(\d+)(?:[.\w-]*)", version)
        if not match or tuple(map(int, match.groups())) < (5, 3):
            raise RuntimeError(f"Threadmill preflight requires strace >= 5.3: {version}")

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
                self._cache_mode,
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
        runtime_env = {"HOME": _REMOTE_HOME.as_posix(), "TMPDIR": _REMOTE_TMP.as_posix()}
        diagnostic = await self.exec_as_agent(
            environment,
            command=(
                f"{shlex.quote(_REMOTE_BINARY.as_posix())} "
                f"-C {shlex.quote(self._workspace.as_posix())} "
                f"-config {shlex.quote(_REMOTE_CONFIG.as_posix())} -exec-doctor "
                f"| tee {logs}/preflight.json"
            ),
            env=runtime_env,
            cwd=self._workspace.as_posix(),
            timeout_sec=60,
        )
        try:
            preflight = json.loads(diagnostic.stdout or "")
        except (ValueError, TypeError) as error:
            raise RuntimeError("Threadmill preflight missing exec_dependency_tracing") from error
        if preflight.get("exec_dependency_tracing") is not True:
            raise RuntimeError("Threadmill preflight failed: exec_dependency_tracing")
        expected_enabled = self._cache_mode != "off"
        if preflight.get("exec_dependency_tracing_enabled") is not expected_enabled:
            raise RuntimeError("Threadmill preflight failed: exec_dependency_tracing_enabled")
        command = (
            "set +e; "
            f"{shlex.quote(_REMOTE_BINARY.as_posix())} "
            f"-C {shlex.quote(self._workspace.as_posix())} "
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
            "--exclude='./.floor' --exclude='./.floors' --exclude='./.replaced' "
            f"-cpf {logs}/vfs-state.tar . || collect_status=$?; fi; "
            "if [ \"$status\" -eq 0 ] && [ \"$collect_status\" -ne 0 ]; then "
            "status=$collect_status; fi; "
            "exit \"$status\""
        )
        await self.exec_as_agent(
            environment,
            command=command,
            env=runtime_env,
            cwd=self._workspace.as_posix(),
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


class Threadmill(ThreadmillRunner, BaseInstalledAgent):
    """Harbor entry point."""
