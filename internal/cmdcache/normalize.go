package cmdcache

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Diagnostics only. Raw command keys remain unchanged until shadow evidence
// justifies semantic normalization. API: https://pkg.go.dev/mvdan.cc/sh/v3@v3.12.0/syntax
type commandSignature struct{ raw, ast, producer string }

func signature(key Key) commandSignature {
	sig := commandSignature{raw: key.index()}
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(key.Command), "")
	if err != nil {
		return sig
	}
	printKey := func(node syntax.Node) string {
		var out strings.Builder
		if err := syntax.NewPrinter().Print(&out, node); err != nil {
			return ""
		}
		key.Command = strings.TrimSpace(out.String())
		return key.index()
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
