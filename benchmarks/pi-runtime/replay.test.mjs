import assert from "node:assert/strict";
import { test } from "node:test";
import { validateTrace } from "./replay.mjs";

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
