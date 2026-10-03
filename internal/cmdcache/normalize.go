package cmdcache

import (
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Approximate similarities remain diagnostic; producer-only reuse is not enabled.
type commandSignature struct{ raw, ast, producer string }

// Normalize only single-line static calls/pipelines. Bash exposes source text
// through BASH_EXECUTION_STRING, BASH_COMMAND, LINENO, traps and dynamic code;
// their syntax retains an exact raw key. Execution always uses Key.Command.
// API: https://pkg.go.dev/mvdan.cc/sh/v3@v3.12.0/syntax
func normalizedCommand(source string) string {
	if strings.ContainsAny(source, "\r\n") {
		return ""
	}
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(source), "")
	if err != nil || len(file.Stmts) != 1 {
		return ""
	}
	safe := true
	syntax.Walk(file, func(node syntax.Node) bool {
		if !safe {
			return false
		}
		// Keep the original command-word qualification. Quoted declare/let
		// parse as CallExpr instead of DeclClause/LetClause; do not turn them
		// into ordinary literal calls before checking source observability.
		if call, ok := node.(*syntax.CallExpr); ok && len(call.Args) > 0 && call.Args[0].Lit() == "" {
			safe = false
			return false
		}
		if word, ok := node.(*syntax.Word); ok {
			normalizeStaticQuote(word)
		}
		return true
	})
	if !safe {
		return ""
	}
	syntax.Walk(file, func(node syntax.Node) bool {
		if node == nil || !safe {
			return false
		}
		switch node := node.(type) {
		case *syntax.File, *syntax.Word, *syntax.Lit, *syntax.SglQuoted, *syntax.DblQuoted:
		case *syntax.Stmt:
			safe = !node.Negated && !node.Background && !node.Coprocess
		case *syntax.BinaryCmd:
			safe = node.Op == syntax.Pipe || node.Op == syntax.PipeAll
		case *syntax.Redirect:
			safe = node.Hdoc == nil
			if node.N != nil {
				_, err := strconv.ParseUint(node.N.Value, 10, 64)
				safe = safe && err == nil
			}
		case *syntax.CallExpr:
			if len(node.Assigns) != 0 || len(node.Args) == 0 {
				safe = false
				break
			}
			name := node.Args[0].Lit()
			safe = name != "" && !strings.ContainsAny(name, "*?[]{}~\\")
			// This limits normalization, not dependency inference or cache
			// eligibility. These builtins can observe or execute shell source.
			switch name {
			case ".", "source", "eval", "trap", "set", "shopt",
				"builtin", "command", "fc", "history", "enable",
				"read", "unset", "mapfile", "readarray", "getopts":
				safe = false
			case "printf", "test", "[":
				// Match the existing segments.go variable-target boundary.
				// Bash may evaluate an indexed target as arithmetic, including
				// shell variables named without a parameter-expansion node.
				for _, arg := range node.Args[1:] {
					// Complex quoting may hide -v; do not decode shell words.
					literal := arg.Lit()
					if literal == "" || strings.HasPrefix(literal, "-v") {
						safe = false
						break
					}
				}
			}
		default:
			// Expansions, functions, compound commands and unknown new nodes
			// retain raw keys rather than assuming formatting equivalence.
			safe = false
		}
		return safe
	})
	if !safe {
		return ""
	}
	var out strings.Builder
	if err := syntax.NewPrinter().Print(&out, file); err != nil {
		return ""
	}
	return strings.TrimSpace(out.String())
}

// Quote returns unquoted text only when it is safe as a shell word. Restrict
// extraction to plain, single-part quotes: no expansion or escape decoder.
// https://pkg.go.dev/mvdan.cc/sh/v3@v3.12.0/syntax#Quote
func normalizeStaticQuote(word *syntax.Word) {
	if len(word.Parts) != 1 {
		return
	}
	var value string
	switch quoted := word.Parts[0].(type) {
	case *syntax.SglQuoted:
		if quoted.Dollar {
			return
		}
		value = quoted.Value
	case *syntax.DblQuoted:
		if quoted.Dollar || len(quoted.Parts) != 1 {
			return
		}
		literal, ok := quoted.Parts[0].(*syntax.Lit)
		if !ok {
			return
		}
		value = literal.Value
	default:
		return
	}
	quoted, err := syntax.Quote(value, syntax.LangBash)
	if err != nil || quoted != value {
		return
	}
	word.Parts = []syntax.WordPart{&syntax.Lit{
		ValuePos: word.Pos(), ValueEnd: word.End(), Value: value,
	}}
}

func signature(key Key) commandSignature {
	// Do not call index: runtime normalization and diagnostics share only the
	// non-recursive hash primitive, never each other's normalization.
	sig := commandSignature{raw: key.commandIndex("raw\n" + key.Command)}
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(key.Command), "")
	if err != nil {
		return sig
	}
	printKey := func(node syntax.Node) string {
		var out strings.Builder
		if err := syntax.NewPrinter().Print(&out, node); err != nil {
			return ""
		}
		return key.commandIndex("ast\n" + strings.TrimSpace(out.String()))
	}
	sig.ast = printKey(file)
	if len(file.Stmts) == 1 {
		stmt := file.Stmts[0]
		if pipe, ok := stmt.Cmd.(*syntax.BinaryCmd); ok && (pipe.Op == syntax.Pipe || pipe.Op == syntax.PipeAll) && !stmt.Negated && !stmt.Background && len(stmt.Redirs) == 0 {
			sig.producer = printKey(pipe.X)
		}
	}
	return sig
}

func (c *Cache) rememberCommand(key Key) {
	sig := signature(key)
	if sig.ast == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, previous := range c.near {
		if previous.raw == sig.raw {
			return
		}
	}
	// Fixed diagnostic window: no unbounded command retention or index scan.
	if len(c.near) == 256 {
		copy(c.near, c.near[1:])
		c.near[len(c.near)-1] = sig
	} else {
		c.near = append(c.near, sig)
	}
}

func (c *Cache) recordNearMiss(key Key) {
	sig := signature(key)
	if sig.ast == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, previous := range c.near {
		if previous.raw == sig.raw {
			continue
		}
		if previous.ast == sig.ast {
			c.stats.NearMissAST++
			return
		}
		if sig.producer != "" && (sig.producer == previous.ast || sig.producer == previous.producer) || previous.producer != "" && previous.producer == sig.ast {
			c.stats.NearMissPipeline++
			return
		}
	}
}
