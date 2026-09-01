#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 1 || $# -gt 2 ]]; then
  echo "usage: $0 SEQUENTIAL_SUMMARY [COLD_SUMMARY]" >&2
  exit 2
fi

sequential=$1

jq -r '
  ["case", "group", "mode", "candidates", "selected", "described", "model_calls", "tool_calls",
   "attach_ops", "status_ops", "content_ops", "subgraph_ops", "nav", "l2_commits", "collapse",
   "resets", "peak_input", "tool_errors", "subscriptions", "seconds", "input", "cached", "cache_pct",
   "assert_failures", "degraded", "error"],
  (.cases[] |
    [
      .spec.id,
      (.spec.group // "control"),
      (.spec.mode // "query"),
      .metrics.memory_organizer_candidates,
      (.selected | length),
      (((.subgraph.admission // "") != "" and (.subgraph.scope // "") != "")),
      (.metrics.model.completed // 0),
      (.metrics.tool_calls // 0),
      # Membership, status and content changes are different acts: one query moved
      # 47 nodes into a subgraph while flipping exactly two statuses. A single
      # node_ops column reads those as the same amount of work.
      (((.graph_delta.nodes_added // []) | length)
        + ([.graph_delta.nodes_changed[]? | select((.before.subgraph_ids // []) != (.after.subgraph_ids // []))] | length)),
      ([.graph_delta.nodes_changed[]? | select(.before.status != .after.status)] | length),
      ([.graph_delta.nodes_changed[]? | select(.before.statement != .after.statement)] | length),
      (((.graph_delta.subgraphs_added // []) | length) + ((.graph_delta.subgraphs_deleted // []) | length) + ((.graph_delta.subgraphs_changed // []) | length)),
      .discipline.navigation_calls,
      .discipline.membership_commits_without_l3,
      .discipline.collapse_calls,
      .discipline.session_resets,
      .discipline.peak_input_tokens,
      .metrics.tool.errors,
      (.subscriptions_after | join(",")),
      ((.duration / 1000000000) | floor),
      .metrics.input_tokens,
      .metrics.cached_tokens,
      ((100 * .metrics.total_cache_hit_rate) | floor),
      ((.assertion_failures // []) | join("; ")),
      ((.degradation // []) | join("; ")),
      (.error // "")
    ]
  ) | @tsv
' "$sequential"

echo
jq -r '
  "subscription projection bytes (final case): flat=\((.cases[-1].projection_cost.flat_bytes // 0)) grouped=\((.cases[-1].projection_cost.grouped_bytes // 0)) attribution=\(.subscription_attribution) session_reset=\(.organizer_session_reset)"
' "$sequential"

if [[ $# -eq 2 ]]; then
  cold=$2
  sequential_model=$(jq -r '.model' "$sequential")
  cold_model=$(jq -r '.model' "$cold")
  if [[ "$sequential_model" != "$cold_model" ]]; then
    echo >&2
    echo "REFUSING COMPARISON: sequential ran on $sequential_model, cold ran on $cold_model." >&2
    echo "A cold-start control only isolates the sequential effect when both runs use the same model." >&2
    exit 3
  fi
  echo
  # Only the control group is comparable: the cold run does not have the extended
  # scenarios, and the first sequential query is the prune step the cold run skips.
  jq -n -r --slurpfile sequential "$sequential" --slurpfile cold "$cold" '
    def control($cases): [$cases[] | select((.spec.group // "control") == "control" and (.spec.mode // "query") == "query")];
    def totals($cases): {
      seconds: ((($cases | map(.duration) | add) // 0) / 1000000000),
      selected: (($cases | map(.selected | length) | add) // 0),
      model_calls: (($cases | map(.metrics.model.completed // 0) | add) // 0),
      tool_calls: (($cases | map(.metrics.tool_calls // 0) | add) // 0),
      input: (($cases | map(.metrics.input_tokens) | add) // 0),
      cached: (($cases | map(.metrics.cached_tokens) | add) // 0),
      uncached: (($cases | map(.metrics.input_tokens - .metrics.cached_tokens) | add) // 0),
      peak_input: (($cases | map(.discipline.peak_input_tokens // 0) | max) // 0)
    };
    ["mode", "seconds", "selected", "model_calls", "tool_calls", "input", "cached", "uncached", "peak_input"],
    (["cold"] + (totals(control($cold[0].cases)) | [.seconds, .selected, .model_calls, .tool_calls, .input, .cached, .uncached, .peak_input])),
    (["sequential-matched"] + (totals(control($sequential[0].cases)[1:]) | [.seconds, .selected, .model_calls, .tool_calls, .input, .cached, .uncached, .peak_input]))
    | @tsv
  '
fi
