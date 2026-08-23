package agent

import (
	"context"

	"github.com/KDZZZZZZ/threadmill/internal/env"
	agenttool "github.com/KDZZZZZZ/threadmill/internal/tool"
)

func firstOverlay(overlay []FileOverlay) FileOverlay {
	if len(overlay) == 0 {
		return FileOverlay{}
	}
	return overlay[0]
}

func applyFileOverlay(loop *Loop, overlay FileOverlay) error {
	if loop == nil {
		return nil
	}
	if overlay.Prompts.Compact != "" {
		loop.compactPrompt = overlay.Prompts.Compact
	}
	if overlay.Prompts.CompactJSONReminder != "" {
		loop.compactJSONReminder = overlay.Prompts.CompactJSONReminder
	}
	if overlay.Prompts.DropContextPressure != "" {
		loop.dropContextReminder = overlay.Prompts.DropContextPressure
	}
	if overlay.Prompts.OrganizeQuery != "" {
		loop.organizeQueryInstruction = overlay.Prompts.OrganizeQuery
	}
	loop.curation = overlay.Curation.Normalized()
	if len(overlay.Tools) == 0 {
		return nil
	}
	listed := overlayToolDescriptions(listedToolsLocked(loop), overlay.Tools)
	tools, definitions, err := prepareTools(listed)
	if err != nil {
		return err
	}
	loop.tools = tools
	loop.definitions = definitions
	return nil
}

func (l *Loop) organizeQueryText() string {
	if l == nil {
		return ""
	}
	return l.organizeQueryInstruction
}

type describedTool struct {
	description string
	inner       agenttool.Tool
}

var (
	_ agenttool.Tool      = describedTool{}
	_ agenttool.EnvBinder = describedTool{}
	_ requesterBinder     = describedTool{}
	_ hidden              = describedTool{}
)

func overlayToolDescriptions(tools []agenttool.Tool, catalog FileToolCatalog) []agenttool.Tool {
	if len(catalog) == 0 {
		return tools
	}
	out := make([]agenttool.Tool, len(tools))
	for i, tool := range tools {
		spec, ok := catalog[tool.Definition().Name]
		if !ok || spec.Description == "" {
			out[i] = tool
			continue
		}
		out[i] = describedTool{description: spec.Description, inner: tool}
	}
	return out
}

func (t describedTool) Definition() agenttool.Definition {
	def := t.inner.Definition()
	def.Description = t.description
	return def
}

func (t describedTool) Execute(ctx context.Context, call agenttool.Call) (agenttool.Output, error) {
	return t.inner.Execute(ctx, call)
}

func (t describedTool) BindEnv(e env.Env) agenttool.Tool {
	if binder, ok := t.inner.(agenttool.EnvBinder); ok {
		t.inner = binder.BindEnv(e)
	}
	return t
}

func (t describedTool) BindRequester(loop *Loop) {
	if binder, ok := t.inner.(requesterBinder); ok {
		binder.BindRequester(loop)
	}
}

func (t describedTool) Hidden() bool {
	return toolHidden(t.inner)
}

func organizerFromTool(tool agenttool.Tool) *Loop {
	for tool != nil {
		switch typed := tool.(type) {
		case *organizeSubgraphTool:
			return typed.organizer
		case describedTool:
			tool = typed.inner
		default:
			return nil
		}
	}
	return nil
}
