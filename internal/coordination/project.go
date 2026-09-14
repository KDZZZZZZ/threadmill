package coordination

import (
	"errors"
	"fmt"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

// installProjectInput uses the same prepared input as an isolated task. Only
// the live workspace changes; outputs and input candidates remain snapshots.
func (r *runner) installProjectInput(input *InputProgress, workspace string) error {
	if !input.Started {
		if err := r.stores.Memory.Restore(r.task.Env.ID, input.MemoryRef); err != nil {
			return err
		}
	}
	if err := r.stores.DiscardFiles(workspace); err != nil {
		return err
	}
	if err := r.stores.Files.CreateEnvironment(input.FilesRef, workspace); err != nil {
		return err
	}
	if err := r.stores.Files.BindProject(workspace); err != nil {
		return err
	}
	if !input.Started {
		sources := make([]string, 0, len(input.Sources))
		for _, source := range input.Sources {
			sources = append(sources, source.FilesRef)
		}
		if _, err := r.stores.Files.InstallProject(input.FilesRef, sources); err != nil {
			return err
		}
	}
	if input.Started {
		return nil
	}
	input.Started = true
	return r.saveInput(*input)
}

func projectSourceID(node Node) string { return node.ID + ":project" }

// Existing project files have no associated agent claims. They enter the same
// input protocol with an empty memory snapshot, alongside declared graph inputs.
func (r *runner) projectSource(node Node, capture bool) (Output, error) {
	id := projectSourceID(node)
	output := Output{
		Node:      Node{ID: id, TaskID: r.task.ID, Role: node.Role},
		FilesRef:  id + ":files",
		MemoryRef: id + ":memory",
		Report:    "真实目录现有内容的固定输入快照；没有附带的实现或验收结论。",
	}
	if r.stores.Files == nil {
		return Output{}, fmt.Errorf("coordination: real directory requires a file store")
	}
	if err := r.stores.Files.Restore(output.FilesRef); errors.Is(err, vfs.ErrUnknownEnvironment) {
		if !capture {
			return Output{}, err
		}
		if err := r.stores.Files.ArchiveProject(output.FilesRef); err != nil {
			return Output{}, err
		}
	} else if err != nil {
		return Output{}, err
	}
	if err := r.stores.Memory.SaveSnapshot(output.MemoryRef, ctxgraph.Graph{}); err != nil {
		return Output{}, err
	}
	return output, nil
}
