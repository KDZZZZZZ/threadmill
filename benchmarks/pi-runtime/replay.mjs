// Import the pinned build's native tool definitions, using their default operations.
// No AgentSession or model provider is created.
import { spawn } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import { dirname, join, resolve } from "node:path";
import { pathToFileURL } from "node:url";
import { parseArgs } from "node:util";

export function validateTrace(trace) {
  if (trace.version !== 1 || !Array.isArray(trace.agents) || trace.agents.length === 0) {
    throw new Error("trace requires version 1 and at least one agent");
  }
  if (!/^[0-9a-f]{40}$/.test(trace.fixture?.commit ?? "")) throw new Error("fixture commit must be pinned");
  if (!["synthetic", "repository"].includes(trace.fixture.kind)) throw new Error("unknown fixture kind");
  const seen = new Set();
  for (const agent of trace.agents) {
    if (!/^[a-zA-Z0-9_-]+$/.test(agent.id) || seen.has(agent.id)) throw new Error("invalid or duplicate agent ID");
    seen.add(agent.id);
    if (!Array.isArray(agent.operations)) throw new Error("agent requires an operations array");
    for (const op of agent.operations) {
      if (!["think", "read", "write", "list", "bash"].includes(op.op)) throw new Error(`unknown operation ${op.op}`);
      if (op.op === "think" && (!Number.isSafeInteger(op.duration_ns ?? 0) || (op.duration_ns ?? 0) < 0)) {
        throw new Error("invalid think duration");
      }
      if (["read", "write", "list"].includes(op.op)) {
        if (!op.path || op.path.startsWith("/") || op.path.split("/").includes("..") || op.path.split("/")[0] === ".git") {
          throw new Error(`unsafe workspace path ${op.path}`);
        }
      }
      if (op.op === "bash" && (!op.command || !Number.isInteger(op.expected_exit ?? 0) || (op.expected_exit ?? 0) < 0 || (op.expected_exit ?? 0) > 255)) {
        throw new Error("invalid bash operation");
      }
    }
  }
}

const nanoseconds = () => process.hrtime.bigint();
const elapsed = (start) => Number(nanoseconds() - start);
const sha256 = (text) => createHash("sha256").update(text).digest("hex");

function git(repo, ...args) {
  return new Promise((done, reject) => {
    const child = spawn("git", ["-c", "gc.auto=0", "-C", repo, ...args], {
      env: { ...process.env, GIT_TERMINAL_PROMPT: "0" }, stdio: ["ignore", "pipe", "pipe"],
    });
    const output = [];
    child.stdout.on("data", (data) => output.push(data));
    child.stderr.on("data", (data) => output.push(data));
    child.on("error", reject);
    child.on("close", (code) => {
      const text = Buffer.concat(output).toString();
      if (code === 0) done(text.trim());
      else reject(new Error(`git ${args.join(" ")}: exit ${code}: ${text}`));
    });
  });
}

export async function replay(options) {
  const raw = await readFile(options.traceIn);
  const trace = JSON.parse(raw);
  validateTrace(trace);
  const pin = JSON.parse(await readFile(new URL("./pi-pin.json", import.meta.url)));
  const sourceCommit = await git(options.piSource, "rev-parse", "HEAD");
  if (sourceCommit !== pin.commit) throw new Error(`Pi source ${sourceCommit} differs from pin ${pin.commit}`);
  if (await git(options.piSource, "diff", "--name-only", "HEAD")) throw new Error("Pi tracked sources are modified");
  const build = JSON.parse(await readFile(join(options.piSource, ".threadmill-benchmark-build.json")));
  if (build.pi_commit !== pin.commit || !build.offline_build) throw new Error("Pi build manifest does not match pinned offline build");
  for (const [file, digest] of Object.entries(build.tool_files)) {
    if (sha256(await readFile(join(options.piSource, file))) !== digest) throw new Error(`Pi compiled native tool differs from build manifest: ${file}`);
  }
  if (await git(options.repo, "rev-parse", "HEAD") !== trace.fixture.commit) throw new Error("fixture commit differs from trace");
  if (await git(options.repo, "status", "--porcelain", "--untracked-files=all")) throw new Error("fixture must be clean");
  const exports = {};
  const setup = nanoseconds();
  for (const [operation, spec] of Object.entries(pin.tools)) {
    const [file, factory] = spec.split(":");
    const module = await import(pathToFileURL(join(resolve(options.piSource), file)));
    if (typeof module[factory] !== "function") throw new Error(`native Pi ${factory} missing`);
    exports[operation] = module[factory];
  }
  await mkdir(options.workdir, { recursive: true });
  await git(options.repo, "config", "gc.auto", "0");
  const result = {
    version: 1, backend: options.sharedCwd ? "pi-shared-cwd" : "pi-worktree", pi_commit: pin.commit,
    fixture_commit: trace.fixture.commit, trace_sha256: sha256(raw), agents: trace.agents.length,
    serial: Boolean(trace.serial), wall_ns: 0, commands_per_second: 0, errors: 0,
    cache_oracle_mismatches: 0, operations: [], latency: {}, setup_ns: elapsed(setup),
    native_tool_semantics: { read: "default truncation", list: "native createLsToolDefinition; default limit 500", bash: "unsandboxed native spawn" },
    pi_build: build,
  };
  let sharedCollect = Promise.resolve();
  const record = (row) => {
    if (row.error) result.errors++;
    result.operations.push(row);
  };
  const agentWork = async (agent) => {
    const cwd = options.sharedCwd ? resolve(options.repo) : join(resolve(options.workdir), agent.id);
    const branch = `bench-${agent.id}`;
    const fork = nanoseconds();
    let created = false;
    try {
      if (!options.sharedCwd) await git(options.repo, "worktree", "add", "--quiet", "-b", branch, cwd, trace.fixture.commit);
      created = true;
      record({ agent: agent.id, index: -1, op: "fork", duration_ns: elapsed(fork) });
      const tools = Object.fromEntries(Object.entries(exports).map(([name, factory]) => [name, factory(cwd)]));
      for (const [index, op] of agent.operations.entries()) {
        const start = nanoseconds();
        const row = { agent: agent.id, index, op: op.op, duration_ns: 0 };
        try {
          if (op.op === "think") {
            if ((op.duration_ns ?? 0) > 0) await new Promise((done) => setTimeout(done, op.duration_ns / 1e6));
          } else {
            const params = op.op === "bash" ? { command: op.command, timeout: 120 }
              : op.op === "write" ? { path: op.path, content: op.content ?? "" } : { path: op.path };
            const output = await tools[op.op].execute(`${agent.id}-${index}`, params);
            if (op.op === "bash") {
              row.exit_code = output.structuredContent?.exit_code;
              if (!Number.isInteger(row.exit_code)) throw new Error("native bash returned no structured exit code");
              row.output = output.structuredContent.output;
              row.output_sha256 = sha256(row.output);
              row.go_cached_results = (row.output.match(/\(cached\)/g) ?? []).length;
              if (row.exit_code !== (op.expected_exit ?? 0)) throw new Error(`exit code ${row.exit_code}, want ${op.expected_exit ?? 0}`);
            } else if (output.isError) throw new Error(JSON.stringify(output));
          }
        } catch (error) { row.error = error.message; }
        row.duration_ns = elapsed(start);
        record(row);
      }
      const start = nanoseconds();
      const collect = async () => {
        await git(cwd, "add", "-A");
        return git(cwd, "-c", "user.name=Pi benchmark", "-c", "user.email=benchmark@example.invalid", "commit", "--allow-empty", "--quiet", "-m", `Collect ${agent.id}`);
      };
      let error;
      try {
        if (options.sharedCwd) {
          sharedCollect = sharedCollect.then(collect);
          await sharedCollect;
        } else await collect();
      } catch (err) { error = err.message; }
      record({ agent: agent.id, index: agent.operations.length, op: "collect", duration_ns: elapsed(start), ...(error ? { error } : {}) });
    } catch (error) {
      record({ agent: agent.id, index: -1, op: "fork", duration_ns: elapsed(fork), error: error.message });
    } finally {
      const start = nanoseconds();
      let error;
      try {
        if (created && !options.sharedCwd) await git(options.repo, "worktree", "remove", "--force", cwd);
      } catch (err) { error = err.message; }
      record({ agent: agent.id, index: agent.operations.length + 1, op: "release", duration_ns: elapsed(start), ...(error ? { error } : {}) });
    }
  };
  const started = nanoseconds();
  if (trace.serial) {
    for (const agent of trace.agents) await agentWork(agent);
  } else await Promise.all(trace.agents.map(agentWork));
  result.wall_ns = elapsed(started);
  for (const op of ["fork", "think", "read", "write", "list", "bash", "collect", "release"]) {
    const samples = result.operations.filter((row) => row.op === op).map((row) => row.duration_ns).sort((a, b) => a - b);
    if (samples.length) result.latency[op] = { count: samples.length, p50_ns: samples[Math.min(Math.floor(samples.length * 0.50), samples.length - 1)], p95_ns: samples[Math.min(Math.floor(samples.length * 0.95), samples.length - 1)] };
  }
  result.commands_per_second = (result.latency.bash?.count ?? 0) * 1e9 / result.wall_ns;
  result.operations.sort((a, b) => a.agent.localeCompare(b.agent) || a.index - b.index);
  result.rss_peak_bytes = process.resourceUsage().maxRSS * 1024;
  return result;
}

if (process.argv[1] && pathToFileURL(resolve(process.argv[1])).href === import.meta.url) {
  const { values } = parseArgs({ options: {
    "pi-source": { type: "string" }, repo: { type: "string" }, workdir: { type: "string" },
    "trace-in": { type: "string" }, "json-out": { type: "string" }, "shared-cwd": { type: "boolean", default: false },
  } });
  const options = { piSource: values["pi-source"], repo: values.repo, workdir: values.workdir, traceIn: values["trace-in"], sharedCwd: values["shared-cwd"] };
  try {
    if ([options.piSource, options.repo, options.workdir, options.traceIn].some((value) => !value)) throw new Error("--pi-source, --repo, --workdir and --trace-in are required");
    const result = await replay(options);
    if (values["json-out"]) {
      await mkdir(dirname(resolve(values["json-out"])), { recursive: true });
      await writeFile(values["json-out"], JSON.stringify(result, null, 2) + "\n");
    } else process.stdout.write(JSON.stringify(result) + "\n");
    process.exitCode = result.errors ? 1 : 0;
  } catch (error) { process.stderr.write(`pi-runtime: ${error.message}\n`); process.exitCode = 1; }
}
