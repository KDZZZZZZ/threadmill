"""Stage DeepSWE tasks with worktree patch collection for commit-free agents."""

import json
import re
import shutil
import sys
import tomllib
from pathlib import Path


def prepare_task(source: Path, destination: Path) -> None:
    text = (source / "task.toml").read_text()
    config = tomllib.loads(text)
    for collect in config["verifier"]["collect"]:
        command = collect["command"]
        pattern = r"git diff --binary ([0-9a-f]{7,40}) HEAD"
        match = re.search(pattern, command)
        if match is None:
            raise ValueError(f"unsupported DeepSWE patch collector: {source}")
        # Use a separate index so new files enter the patch without a commit or
        # any mutation to the project's HEAD or index. Keep the grader intact.
        replacement = (
            '(set -e; index=$(mktemp); rm -f "$index"; '
            'export GIT_INDEX_FILE="$index"; '
            'trap \'rm -f "$index"\' EXIT; '
            'git read-tree HEAD && git add --all -- . && '
            f'git diff --cached --binary {match.group(1)})'
        )
        updated = command[:match.start()] + replacement + command[match.end():]
        # Upstream uses a single-line TOML basic string; refuse other layouts.
        old = f"command = {json.dumps(command)}"
        if text.count(old) != 1:
            raise ValueError(f"unsupported collector serialization: {source}")
        text = text.replace(old, f"command = {json.dumps(updated)}", 1)
    shutil.copytree(source, destination)
    (destination / "task.toml").write_text(text)


def main() -> None:
    source, destination = map(Path, sys.argv[1:])
    if (source / "task.toml").is_file():
        prepare_task(source, destination / source.name)
        print(destination / source.name)
    else:
        tasks = destination / "tasks"
        for task in sorted(source.iterdir()):
            if (task / "task.toml").is_file():
                prepare_task(task, tasks / task.name)
        print(tasks)


if __name__ == "__main__":
    main()
