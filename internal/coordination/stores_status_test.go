package coordination

import (
	"testing"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
)

func TestReportNodeStatus(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   string
	}{
		{
			name:   "pass with command evidence accepted",
			output: "结论: PASS\n门禁证据：\ngo test ./... 退出码 0",
			want:   ctxgraph.NodeStatusAccepted,
		},
		{
			name:   "pass without command evidence disputed",
			output: "结论: PASS\n全部验收项通过。",
			want:   ctxgraph.NodeStatusDisputed,
		},
		{
			name: "pass with source evidence accepted",
			output: "结论: PASS\nC1: PASS\n" +
				"证据锚: https://example.com/spec#api-contract\n" +
				"原始观察: API contract requires a stable request identifier\n" +
				"适用范围: specification revision 4",
			want: ctxgraph.NodeStatusAccepted,
		},
		{
			name: "pass with file evidence accepted",
			output: "结论: PASS\nC1: PASS\n" +
				"证据锚: internal/coordination/stores.go:133\n" +
				"原始观察: reportNodeStatus checks report evidence\n" +
				"适用范围: current task snapshot",
			want: ctxgraph.NodeStatusAccepted,
		},
		{
			name:   "anchor without observation disputed",
			output: "结论: PASS\n证据锚: https://example.com/spec",
			want:   ctxgraph.NodeStatusDisputed,
		},
		{
			name: "record without scope disputed",
			output: "结论: PASS\n证据锚: https://example.com/spec\n" +
				"原始观察: the source states the required behavior",
			want: ctxgraph.NodeStatusDisputed,
		},
		{
			name: "fields from different records do not combine",
			output: "结论: PASS\n契约: C1\n证据锚: https://example.com/spec\n\n" +
				"契约: C2\n原始观察: a different claim\n适用范围: another revision",
			want: ctxgraph.NodeStatusDisputed,
		},
		{
			name:   "bare source URL disputed",
			output: "结论: PASS\n参见 https://example.com/spec",
			want:   ctxgraph.NodeStatusDisputed,
		},
		{
			name:   "fail without evidence disputed",
			output: "结论: FAIL\n缺少 fallback 路径。",
			want:   ctxgraph.NodeStatusDisputed,
		},
		{
			name:   "inconclusive without evidence disputed",
			output: "结论: INCONCLUSIVE\n缺少依赖。",
			want:   ctxgraph.NodeStatusDisputed,
		},
		{
			name: "fail with evidence accepted",
			output: "结论: FAIL\nC1: FAIL\n" +
				"证据锚: source:vendor-guide#unsupported-mode\n" +
				"原始观察: documented modes omit the required value\n" +
				"适用范围: vendor guide 2026-08-31",
			want: ctxgraph.NodeStatusAccepted,
		},
		{
			name:   "no verdict line stays accepted",
			output: "任务未完成：context canceled",
			want:   ctxgraph.NodeStatusAccepted,
		},
		{
			name:   "english exit code marker counts as evidence",
			output: "结论: PASS\npytest -q  exit code 0",
			want:   ctxgraph.NodeStatusAccepted,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reportNodeStatus(tt.output); got != tt.want {
				t.Errorf("reportNodeStatus(%q) = %q, want %q", tt.output, got, tt.want)
			}
		})
	}
}
