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
			name:   "fail always accepted as open defect",
			output: "结论: FAIL\n缺少 fallback 路径。",
			want:   ctxgraph.NodeStatusAccepted,
		},
		{
			name:   "inconclusive accepted as open gap",
			output: "结论: INCONCLUSIVE\n缺少依赖。",
			want:   ctxgraph.NodeStatusAccepted,
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
