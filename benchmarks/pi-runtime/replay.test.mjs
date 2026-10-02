import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { access, mkdtemp, mkdir, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { setTimeout as delay } from "node:timers/promises";
import { promisify } from "node:util";
import { createWorktreeLifecycle, validateTrace } from "./replay.mjs";

const execute = promisify(execFile);
const exists = async (path) => access(path).then(() => true, () => false);
const quote = (value) => `'${value.replaceAll("'", "'\\''")}'`;

async function fixture(root) {
  const repo = join(root, "repo");
  await mkdir(repo);
  const git = (...args) => execute("git", ["-c", "gc.auto=0", "-C", repo, ...args]);
  await git("init", "--quiet");
  await writeFile(join(repo, "source.txt"), "committed input\n");
  await git("add", "source.txt");
  await git("-c", "user.name=Benchmark", "-c", "user.email=benchmark@example.invalid", "commit", "--quiet", "-m", "fixture");
  return repo;
}

const trace = (operation) => ({ version: 1, fixture: { kind: "repository", files: 1, file_bytes: 4096, commit: "a".repeat(40) }, agents: [{ id: "agent-0", operations: [operation] }] });

test("native replay rejects a write to a sibling workspace before invoking tools", () => {
  assert.throws(() => validateTrace(trace({ op: "write", path: "../sibling/file", content: "changed" })), /unsafe workspace/);
});

test("native replay accepts an explicitly expected failing command", () => {
  assert.doesNotThrow(() => validateTrace(trace({ op: "bash", command: "exit 7", expected_exit: 7 })));
});

test("duplicate agent IDs cannot alias worktree branches", () => {
  const input = trace({ op: "think", duration_ns: 0 });
  input.agents.push(input.agents[0]);
  assert.throws(() => validateTrace(input), /duplicate agent ID/);
});

test("real Git add and remove wait for the same common-metadata lifecycle", async () => {
  const root = await mkdtemp(join(tmpdir(), "pi-worktree-lifecycle-"));
  const entered = join(root, "entered"), release = join(root, "release");
  const running = [];
  try {
    const repo = await fixture(root);
    const lifecycle = createWorktreeLifecycle(repo);
    const existing = join(root, "existing"), first = join(root, "first"), second = join(root, "second");
    await lifecycle("add", "--quiet", "-b", "existing", existing, "HEAD");
    await writeFile(join(repo, ".git/hooks/post-checkout"),
      `#!/bin/sh\nprintf entered > ${quote(entered)}\nwhile [ ! -f ${quote(release)} ]; do sleep 0.01; done\n`, { mode: 0o755 });
    running.push(lifecycle("add", "--quiet", "-b", "first", first, "HEAD"));
    for (let count = 0; !(await exists(entered)) && count < 200; count++) await delay(10);
    assert.ok(await exists(entered), "first real Git add did not reach its checkout hook");
    running.push(lifecycle("remove", "--force", existing), lifecycle("add", "--quiet", "-b", "second", second, "HEAD"));
    for (const operation of running) operation.catch(() => {});
    await delay(150);
    assert.ok(await exists(existing), "remove touched common metadata while the first add was active");
    assert.equal(await exists(second), false, "second add touched common metadata while the first add was active");
    await writeFile(release, "continue\n");
    await Promise.all(running);
    assert.equal(await exists(existing), false);
    assert.ok(await exists(first));
    assert.ok(await exists(second));
    await Promise.all([lifecycle("remove", "--force", first), lifecycle("remove", "--force", second)]);
  } finally {
    await writeFile(release, "continue\n");
    await Promise.allSettled(running);
    await rm(root, { recursive: true, force: true });
  }
});

test("a failed real Git worktree operation does not poison later lifecycle requests", async () => {
  const root = await mkdtemp(join(tmpdir(), "pi-worktree-failure-"));
  try {
    const repo = await fixture(root);
    const lifecycle = createWorktreeLifecycle(repo);
    const rejected = lifecycle("add", "--quiet", "-b", "invalid", join(root, "invalid"), "missing-commit");
    const succeeding = lifecycle("add", "--quiet", "-b", "valid", join(root, "valid"), "HEAD");
    const outcomes = await Promise.allSettled([rejected, succeeding]);
    assert.equal(outcomes[0].status, "rejected");
    assert.match(outcomes[0].reason.message, /missing-commit/);
    assert.equal(outcomes[1].status, "fulfilled");
    await lifecycle("remove", "--force", join(root, "valid"));
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});
