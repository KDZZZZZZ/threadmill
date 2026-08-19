# AGENTS.md

This file is binding for every agent working in this repo.

## Agent OS

Threadmill is a lightweight **Agent OS**.

The agent itself only thinks and calls tools. Every file read, file write, code edit, and command execution must go through a single **Tool layer**. The Tool layer connects the agent to an isolated virtual environment:

- The virtual filesystem keeps many agents' file versions with snapshot + delta, so the project is not copied for each agent.
- The virtual execution system queues commands such as `cargo test`, `pytest`, and shell, then maps them onto a limited set of CPU, memory, and execution slots.
- A coordination graph decides which agent runs when.
- A memory graph decides which context that agent sees while it runs.

This is how one personal computer can keep thousands of isolated logical agents, while consuming real files, processes, and compute only on demand.

Agents must not treat the host checkout as private scratch space. They issue tool calls; the Tool layer owns isolation, versioning, scheduling, and resource limits.

## Baseline branch

- Current baseline: `dev-native`.
- Every other branch must land on `dev-native`.
- `dev-native` is the only integration branch until a human explicitly changes this file.

## Merge policy

- Code may enter `dev-native` only through a pull request.
- Direct push, local merge, rebase-onto-baseline, cherry-pick-onto-baseline, and `git merge` into `dev-native` are forbidden.
- Do not merge into `main` unless a human explicitly asks for that in writing.
- Do not fast-forward `dev-native` locally and push. Open a PR.

## PR authorship split

Every PR description must split the change into two labeled sections. Do not mix them.

```markdown
## Human Design

- <only decisions the human explicitly requested or approved>

## Agent Self-Claimed

- <only decisions the agent invented, inferred, or added without an explicit human request>
```

Rules:

- `Human Design` is only for requirements, constraints, names, APIs, architecture, or behavior the human said out loud or wrote down.
- `Agent Self-Claimed` is for everything else: extra files, extra abstractions, dependency choices, refactors, naming the human did not specify, test shape the human did not specify, CI, docs, skill installs, and opportunistic cleanup.
- If a decision is ambiguous, put it under `Agent Self-Claimed`.
- If `Agent Self-Claimed` is empty, write `None`.
- If `Human Design` is empty, the PR is invalid unless the human asked only for an unconstrained cleanup.
- Do not hide agent initiative inside "misc", "chore", or "also".
- PR title may stay short. The split belongs in the PR body, not only in the title.

Example:

```markdown
## Human Design

- Install samber/cc-skills-golang, source-driven-development, mattpocock/tdd, and ponytail into this repo.
- Make the current branch the baseline.
- Allow landing only through PRs.
- Require PRs to separate human design from agent self-claimed work.
- Write the Agent OS model into AGENTS.md.

## Agent Self-Claimed

- Used `npx skills add` and wrote `skills-lock.json`.
- Installed the full `samber/cc-skills-golang` pack, not a subset.
- Resolved TDD to `mattpocock/skills@tdd`.
- Resolved source-driven-development to `addyosmani/agent-skills@source-driven-development`.
```

## Working rules

- Before any Go coding, review, debugging, troubleshooting, or setup task, load the `samber/cc-skills-golang@golang-how-to` skill first — it routes to whichever other Go skills the task needs.
- Use `tdd` for test-first feature or bug work.
- Use `source-driven-development` when implementing against a library or framework.
- Use `ponytail` to keep diffs minimal.
- Do not invent product scope. If the human did not ask for it, either skip it or put it under `Agent Self-Claimed` and keep it small.

## Cross-repository implementation references

When the human asks how to implement something or requests an implementation, inspect the relevant design, implementation, and tests in these repositories as horizontal prior art before choosing Threadmill's approach:

- Pi — [`badlogic/pi-mono`](https://github.com/badlogic/pi-mono)
- deepseek-harness — [`deepseek-ai/deepseek-harness`](https://github.com/deepseek-ai/deepseek-harness)
- Eino — [`cloudwego/eino`](https://github.com/cloudwego/eino)

Use only the portions relevant to the requested subsystem. Compare their behavior and trade-offs rather than copying blindly. Cite the repository and concrete file or commit used when explaining a design. Threadmill's human requirements and local constraints take precedence. If a reference cannot be accessed, say so instead of claiming it was reviewed.

## Installed project skills

- `samber/cc-skills-golang` — Go skill pack, including `golang-how-to`
- `addyosmani/agent-skills@source-driven-development`
- `mattpocock/skills@tdd`
- `dietrichgebert/ponytail`

Skill files live in `.agents/skills/`. Restore with `npx skills experimental_install`.
