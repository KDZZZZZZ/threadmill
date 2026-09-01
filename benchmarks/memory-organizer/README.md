# Memory organizer sequential benchmark

This benchmark measures whether organizer work compounds across later retrievals. It clones one archived environment into memory, then keeps all of the following alive for the entire run:

- one memory graph;
- one organizer conversation;
- one requester and its evolving dynamic subscriptions.

The first query replaces two intentionally broad, stale subscriptions with a compact current-frontier subgraph. The remaining five control queries are the same queries used by the cold-start control. Any opportunistic node cleanup, status correction, deduplication, subgraph description, or subscription change from an earlier query is therefore visible to every later query.

After the control group, an extended group covers the scenarios the control group cannot: a negative control on a topic the graph does not hold, a partial invalidation that must be answered with `detach` rather than a whole-subgraph unsubscribe, a query that shares evidence with an earlier one, and the deep-curation audit (`"mode": "curate"`, driven through the same production entry point compaction uses in production). The extended group is not part of the cold comparison.

## Run

The source `memory.json` is hashed before and after the run and is never used as the writable store.

```bash
./benchmarks/memory-organizer/run.sh /absolute/path/to/memory.json env-30
```

The optional third argument chooses the artifact directory. By default artifacts go to `/tmp/threadmill-memory-organizer/<UTC timestamp>`.

Runtime configuration uses the current binary's built-in prompts plus the normal user configuration. The wrapper supplies an empty project root by default so this repository's development-only model override cannot shadow the user's configured model. The organizer drops its conversation history and re-instantiates from the graph when a request approaches the window (a handoff message naming the subgraphs it has touched replaces the discarded history). Both product switches this harness A/Bs are flags:

```bash
THREADMILL_ORGANIZER_SESSION_RESET=false ./benchmarks/memory-organizer/run.sh ...   # keep one growing session
THREADMILL_SUBSCRIPTION_ATTRIBUTION=true ./benchmarks/memory-organizer/run.sh ...   # group the injection block by source subgraph
```

Override the project layer only when needed:

```bash
THREADMILL_CONFIG_ROOT=/path/to/project \
THREADMILL_CONFIG_PATH=/path/to/override.yaml \
THREADMILL_ORGANIZER_SUBSCRIPTIONS=sg-q-1,sg-q-2 \
THREADMILL_ORGANIZER_TIMEOUT=20m \
./benchmarks/memory-organizer/run.sh /path/to/memory.json env-30 /tmp/my-run
```

Do not put API keys in this directory or in `queries.json`; configure them through the normal user/runtime configuration.

## Observe a running evaluation

The terminal prints one `START` and one `END` line per query. `START` names the mode; `END` includes actual selected-node count, whole-graph size, accumulated subscriptions, duration, organizer tokens, cache rate, largest single request, session resets, assertion failures, degradation flags, and any error.

Every completed query is durable immediately:

- `<case>.json`: full model requests/responses, runtime events, actual target membership, subscriptions before/after, the complete graph delta, and the per-case quality axes (assertion failures, degradation flags, expansion discipline, projection cost);
- `summary.partial.json`: all completed cases without their traces, refreshed after every query, removed once `summary.json` supersedes it;
- `summary.json`: final aggregate metrics, per-case results without traces (the trace stays in the file named by `case_file`), and the source-integrity result;
- `final-graph.json`: the evolved evaluation graph, including incidental organizer cleanup.

Each case carries its own verdicts:

- `assertion_failures`: unmet `must_include` / `must_exclude` / `expect_min_selected` / `expect_max_selected` / `must_stay_subscribed` / `must_share_with` from `queries.json`. Assertions are hypotheses about the archived graph, so they report rather than abort — a failure is a finding to read, not a broken harness.
- `degradation`: an empty selection, a missing admission/scope, a summary copied from the query, or an untouched subgraph revision. Any of these means the case produced a well-formed but empty answer.
- `discipline`: navigation-tool calls, expand/collapse/drop calls, membership committed while the node was still below level 3, session resets, and the largest single request.
- `projection_cost`: the subscriber injection block under both renderings, flat and grouped by source subgraph.

Useful live checks:

```bash
jq '{cases: [.cases[] | {id: .spec.id, selected: (.selected|length), nodes: [.graph_nodes_before,.graph_nodes_after], subscriptions: .subscriptions_after, tokens: .metrics.memory_organizer_tokens, cache: .metrics.total_cache_hit_rate, error} ]}' /tmp/my-run/summary.partial.json

jq '{added: (.graph_delta.nodes_added|length), deleted: (.graph_delta.nodes_deleted|length), changed: (.graph_delta.nodes_changed|length), subgraphs_changed: (.graph_delta.subgraphs_changed|length)}' /tmp/my-run/prune-and-frontier.json
```

For a compact table, and optionally a matched cold-start comparison:

```bash
./benchmarks/memory-organizer/report.sh /tmp/my-run/summary.json
./benchmarks/memory-organizer/report.sh /tmp/my-run/summary.json /tmp/cold-run/summary.json
```

`report.sh` refuses a comparison whose two summaries name different models: a cold-start control only isolates the sequential effect when both runs use the same model. The comparison covers the control group only.

## Modify the workload

Edit `queries.json`. Order is semantic: each query sees all earlier graph mutations and organizer messages. Keep the control query IDs and their order unchanged when comparing against cold-start results; add new scenarios with `"group": "extended"` after them. `must_share_with` may only name an earlier query, and the `partial-invalidation` scenario assumes the first query created `sg-q-3` — with different initial subscriptions, update that ID. A query that tests pruning must explicitly state which dynamic subscription has become invalid; the organizer is not allowed to infer permission to unsubscribe it.

Run `go test ./benchmarks/memory-organizer -count=1` after changing the driver. The benchmark intentionally uses production `organize_subgraph`, memory tools, subscription application, provider, prompts, and event collection rather than a parallel test implementation.
