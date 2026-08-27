package exec

import (
	"strings"
	"testing"
)

func TestSplitCacheCommandRejectsUnsafeShellForms(t *testing.T) {
	tests := map[string]string{
		"arithmetic expansion":   `printf '%s' "$((value=1))"; true`,
		"compact printf assign":  "printf -vvalue %s kept; true",
		"completion state":       "complete -W 'one two' thing; true",
		"coprocess":              "coproc cat; true",
		"extended test state":    "[[ foo =~ (o) ]]; true",
		"loop variable":          "for item in one; do :; done; true",
		"here document":          "cat <<'EOF'\nbody\nEOF\nprintf tail",
		"named descriptor":       ": {output}>result.txt; true",
		"variable introspection": "true; test -v _",
	}
	for name, command := range tests {
		t.Run(name, func(t *testing.T) {
			if segments := splitCacheCommand(command); segments != nil {
				t.Fatalf("splitCacheCommand() = %#v, want whole-shell fallback", segments)
			}
		})
	}
}

func TestSplitCacheCommandBoundsSegmentCount(t *testing.T) {
	if segments := splitCacheCommand(strings.Repeat("true;", maxCacheSegments+1)); segments != nil {
		t.Fatalf("splitCacheCommand() returned %d segments beyond its bound", len(segments))
	}
}

func BenchmarkSplitCacheCommand(b *testing.B) {
	const command = "python3 -m compileall src >/dev/null; " +
		"python3 -m unittest discover -s tests; " +
		"printf 'exit=%d\\n' $?; git status --short"
	b.ReportAllocs()
	for b.Loop() {
		if segments := splitCacheCommand(command); len(segments) != 4 {
			b.Fatalf("segments = %d, want 4", len(segments))
		}
	}
}
