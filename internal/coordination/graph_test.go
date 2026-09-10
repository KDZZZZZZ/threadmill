package coordination

import (
	"reflect"
	"slices"
	"testing"
)

func TestNewGraphsAreIndependent(t *testing.T) {
	t.Parallel()

	first, second := New(), New()
	task := first.AddTask()
	if _, ok := first.Task(task.ID); !ok {
		t.Fatal("task is missing from its graph")
	}
	if got := second.Snapshot(); len(got.Tasks) != 0 || len(got.Nodes) != 0 || len(got.Edges) != 0 {
		t.Fatalf("task leaked into another graph: %#v", got)
	}
}

func TestGraphAddTaskHasExactlyThreeRolesInOrder(t *testing.T) {
	t.Parallel()

	graph := New()
	task := graph.AddTask()

	got := rolesOf(task)
	expected := []string{RolePlanner, RoleExecutor, RoleVerifier}
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("task roles = %v, want %v", got, expected)
	}
	if task.ID == "" {
		t.Fatal("task id is empty")
	}
	for i, node := range task.Sequence() {
		if node.ID == "" || node.TaskID != task.ID || node.Role != expected[i] {
			t.Fatalf("sequence[%d] = %#v, want task %q role %q", i, node, task.ID, expected[i])
		}
	}

	wantEdges := []Edge{
		{From: task.Planner.ID, To: task.Executor.ID},
		{From: task.Executor.ID, To: task.Verifier.ID},
	}
	if got := graph.Snapshot().Edges; !slices.Equal(got, wantEdges) {
		t.Fatalf("task edges = %#v, want %#v", got, wantEdges)
	}
}

func TestGraphConnectsAnyRoleAcrossIndependentTasks(t *testing.T) {
	t.Parallel()
	for _, fromRole := range []string{RolePlanner, RoleExecutor, RoleVerifier} {
		for _, toRole := range []string{RolePlanner, RoleExecutor, RoleVerifier} {
			t.Run(fromRole+" to "+toRole, func(t *testing.T) {
				t.Parallel()
				graph := New()
				first, second := graph.AddTask(), graph.AddTask()
				from, to := nodeByRole(first, fromRole), nodeByRole(second, toRole)
				if err := graph.Connect(from.ID, to.ID); err != nil {
					t.Fatal(err)
				}
				if !containsID(nodeIDs(graph.Incoming(to.ID)), from.ID) {
					t.Fatal("source is missing from incoming nodes")
				}
				if !slices.Contains(graph.Snapshot().Edges, Edge{From: from.ID, To: to.ID}) {
					t.Fatal("dependency is missing from graph snapshot")
				}
				if len(graph.Incoming("unknown")) != 0 {
					t.Fatal("unknown node has dependencies")
				}
			})
		}
	}
}

func rolesOf(task Task) []string {
	nodes := task.Sequence()
	roles := make([]string, 0, len(nodes))
	for _, node := range nodes {
		roles = append(roles, node.Role)
	}
	return roles
}

func nodeByRole(task Task, role string) Node {
	for _, node := range task.Sequence() {
		if node.Role == role {
			return node
		}
	}
	return Node{}
}

func nodeIDs(nodes []Node) []string {
	ids := make([]string, 0, len(nodes))
	for _, node := range nodes {
		ids = append(ids, node.ID)
	}
	return ids
}

func taskIDs(tasks []Task) []string {
	ids := make([]string, 0, len(tasks))
	for _, task := range tasks {
		ids = append(ids, task.ID)
	}
	return ids
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func (g *Graph) taskCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.tasks)
}
