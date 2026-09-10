package coordination

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ctxgraph "github.com/KDZZZZZZ/threadmill/internal/context"
	"github.com/KDZZZZZZ/threadmill/internal/vfs"
)

func TestPublishTaskRetryKeepsSelectedActivation(t *testing.T) {
	t.Parallel()
	for _, nextCompleted := range []bool{false, true} {
		name := "next_active"
		if nextCompleted {
			name = "next_completed"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			graphPath := filepath.Join(dir, "graph.json")
			graph, err := OpenGraph(graphPath)
			if err != nil {
				t.Fatal(err)
			}
			base := filepath.Join(dir, "project")
			stores := Stores{Memory: ctxgraph.NewStore(), Files: vfs.NewStore(base)}
			t.Cleanup(func() { _ = stores.Files.Close() })
			if _, err := executeGraphTool(t, GraphTools(graph), coordOrchestrateName, `{
				"action":"replace_pending","tasks":[{"id":"ongoing","info":"first request","persistent":true}]
			}`); err != nil {
				t.Fatal(err)
			}
			first, _ := graph.Task("ongoing")
			runPublishTask(t, graph, first, stores, "choice.txt", "first", nil)
			if _, err := executeGraphTool(t, GraphTools(graph, stores), coordPublishTaskName, `{"task_id":"ongoing"}`); err == nil {
				t.Fatal("publishing to missing project succeeded")
			}
			if graph.Snapshot().PublishingTaskID != first.ID {
				t.Fatal("failed publication did not retain its intent")
			}

			// Complete the external file stage while the durable intent remains:
			// the same recovery boundary as a failed final graph commit.
			if err := os.Mkdir(base, 0o700); err != nil {
				t.Fatal(err)
			}
			selected, ok := graph.Output(first.Verifier.ID)
			if !ok {
				t.Fatal("first activation has no committed output")
			}
			if _, err := stores.Files.Publish(selected.FilesRef); err != nil {
				t.Fatal(err)
			}
			second, err := graph.Continue(first.ID, "second request")
			if err != nil {
				t.Fatalf("pending publication blocked continuation: %v", err)
			}
			if nextCompleted {
				runPublishTask(t, graph, second, stores, "choice.txt", "second", nil)
			}
			restored, err := OpenGraph(graphPath)
			if err != nil {
				t.Fatal(err)
			}
			projection, err := restored.Snapshot().PromptProjection()
			if err != nil || !strings.Contains(string(projection), `"publishing_node_id":"`+first.Verifier.ID+`"`) {
				t.Fatalf("pending publication projection = %s, %v; want original node", projection, err)
			}
			result, err := executeGraphTool(t, GraphTools(restored, stores), coordPublishTaskName, `{"task_id":"ongoing"}`)
			if err != nil {
				t.Fatalf("retry selected activation 1 while activation 2 exists: %v", err)
			}
			if !strings.Contains(result.Content, `"outcome":"idle"`) {
				t.Fatalf("retry outcome = %s, want selected activation's idle outcome", result.Content)
			}
			if !strings.Contains(result.Content, `"activation":1`) || !strings.Contains(result.Content, `"node_id":"`+first.Verifier.ID+`"`) {
				t.Fatalf("retry receipt = %s, want original activation and node", result.Content)
			}
			if got, err := os.ReadFile(filepath.Join(base, "choice.txt")); err != nil || string(got) != "first" {
				t.Fatalf("retry published %q, %v; want original activation's first snapshot", got, err)
			}
			if restored.Snapshot().PublishingTaskID != "" {
				t.Fatal("successful retry retained pending intent")
			}
			projection, err = restored.Snapshot().PromptProjection()
			if err != nil || !strings.Contains(string(projection), `"published_node_id":"`+first.Verifier.ID+`"`) {
				t.Fatalf("published projection = %s, %v; want original node", projection, err)
			}
			if nextCompleted {
				if _, err := executeGraphTool(t, GraphTools(restored, stores), coordPublishTaskName, `{"task_id":"ongoing"}`); err != nil {
					t.Fatal(err)
				}
				if got, err := os.ReadFile(filepath.Join(base, "choice.txt")); err != nil || string(got) != "second" {
					t.Fatalf("new publication = %q, %v; want current activation's second snapshot", got, err)
				}
			}
		})
	}
}

func TestOpenGraphRejectsPublicationOutsideSelectedOutput(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "graph.json")
	graph, err := OpenGraph(path)
	if err != nil {
		t.Fatal(err)
	}
	task := graph.AddTask()
	stores := Stores{Memory: ctxgraph.NewStore(), Files: vfs.NewStore(t.TempDir())}
	t.Cleanup(func() { _ = stores.Files.Close() })
	runPublishTask(t, graph, task, stores, "choice.txt", "selected", nil)
	if _, err := executeGraphTool(t, GraphTools(graph, stores), coordPublishTaskName, `{"task_id":"`+task.ID+`"}`); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	state["published"].(map[string]any)["files_ref"] = "another-snapshot"
	data, err = json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenGraph(path); err == nil || !strings.Contains(err.Error(), "publication") {
		t.Fatalf("restored publication outside its committed output: %v", err)
	}
}
