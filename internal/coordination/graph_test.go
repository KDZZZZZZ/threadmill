package coordination

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestDefaultIsSingleton(t *testing.T) {
	resetDefault(t)

	if Default() != Default() {
		t.Fatal("Default() returned different graphs")
	}

	first := Default().AddTask()
	if _, ok := Default().Task(first.ID); !ok {
		t.Fatal("task added on Default() is not on the singleton")
	}
}

func TestGraphAddTaskHasExactlyThreeRolesInOrder(t *testing.T) {
	t.Parallel()

	graph := newGraph()
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

	plannerDown := nodeIDs(graph.Downstream(task.Planner.ID))
	if !reflect.DeepEqual(plannerDown, []string{task.Executor.ID}) {
		t.Fatalf("planner downstream = %v, want [%s]", plannerDown, task.Executor.ID)
	}
	executorDown := nodeIDs(graph.Downstream(task.Executor.ID))
	if !reflect.DeepEqual(executorDown, []string{task.Verifier.ID}) {
		t.Fatalf("executor downstream = %v, want [%s]", executorDown, task.Verifier.ID)
	}
	if got := graph.Downstream(task.Verifier.ID); len(got) != 0 {
		t.Fatalf("verifier downstream = %#v, want empty", got)
	}
}

func TestGraphSpawnFromAnyRoleAndJoinToAnyRole(t *testing.T) {
	t.Parallel()

	order := map[string]int{
		RolePlanner:  0,
		RoleExecutor: 1,
		RoleVerifier: 2,
	}
	roles := []string{RolePlanner, RoleExecutor, RoleVerifier}
	for _, fromRole := range roles {
		for _, joinRole := range roles {
			t.Run(fromRole+" to "+joinRole, func(t *testing.T) {
				t.Parallel()

				graph := newGraph()
				parent := graph.AddTask()
				from := nodeByRole(parent, fromRole)
				join := nodeByRole(parent, joinRole)

				child, err := graph.Spawn(from.ID, join.ID)
				if order[joinRole] <= order[fromRole] {
					if !errors.Is(err, ErrJoinCycle) {
						t.Fatalf("Spawn() error = %v, want %v", err, ErrJoinCycle)
					}
					if graph.taskCount() != 1 {
						t.Fatalf("task count = %d, want 1 after rejected Spawn", graph.taskCount())
					}
					return
				}
				if err != nil {
					t.Fatalf("Spawn() error = %v", err)
				}
				if !reflect.DeepEqual(rolesOf(child), roles) {
					t.Fatalf("child roles = %v, want %v", rolesOf(child), roles)
				}
				if child.ID == parent.ID {
					t.Fatal("child reused parent task id")
				}

				if !containsID(nodeIDs(graph.Downstream(from.ID)), child.Planner.ID) {
					t.Fatalf("start %s does not reach child planner", fromRole)
				}
				if !containsID(nodeIDs(graph.Downstream(child.Verifier.ID)), join.ID) {
					t.Fatalf("child verifier does not join %s", joinRole)
				}
			})
		}
	}
}

func TestGraphSpawnUnknownNode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		from func(Task) string
		join func(Task) string
	}{
		{
			name: "unknown from",
			from: func(Task) string { return "missing" },
			join: func(task Task) string { return task.Planner.ID },
		},
		{
			name: "unknown join",
			from: func(task Task) string { return task.Planner.ID },
			join: func(Task) string { return "missing" },
		},
		{
			name: "empty from",
			from: func(Task) string { return "" },
			join: func(task Task) string { return task.Planner.ID },
		},
		{
			name: "empty join",
			from: func(task Task) string { return task.Planner.ID },
			join: func(Task) string { return "" },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			graph := newGraph()
			root := graph.AddTask()
			_, err := graph.Spawn(tt.from(root), tt.join(root))
			if !errors.Is(err, ErrUnknownNode) {
				t.Fatalf("Spawn() error = %v, want %v", err, ErrUnknownNode)
			}
		})
	}
}

func TestGraphSpawnRejectsCrossTreeJoin(t *testing.T) {
	t.Parallel()

	graph := newGraph()
	a := graph.AddTask()
	b := graph.AddTask()
	_, err := graph.Spawn(a.Planner.ID, b.Verifier.ID)
	if !errors.Is(err, ErrJoinCycle) {
		t.Fatalf("Spawn() error = %v, want %v", err, ErrJoinCycle)
	}
	if graph.taskCount() != 2 {
		t.Fatalf("task count = %d, want 2 after rejected Spawn", graph.taskCount())
	}
}

func TestGraphSpawnAllowsSiblingJoin(t *testing.T) {
	t.Parallel()

	graph := newGraph()
	root := graph.AddTask()
	first := mustSpawn(t, graph, root.Planner.ID, root.Verifier.ID)
	second := mustSpawn(t, graph, root.Planner.ID, first.Executor.ID)
	if !containsID(nodeIDs(graph.Downstream(second.Verifier.ID)), first.Executor.ID) {
		t.Fatalf("sibling join missing: downstream=%v", nodeIDs(graph.Downstream(second.Verifier.ID)))
	}
}

func TestGraphSpawnRejectsJoinBeforeSpawnSource(t *testing.T) {
	t.Parallel()

	graph := newGraph()
	root := graph.AddTask()
	child := mustSpawn(t, graph, root.Planner.ID, root.Verifier.ID)
	_, err := graph.Spawn(child.Executor.ID, root.Planner.ID)
	if !errors.Is(err, ErrJoinCycle) {
		t.Fatalf("Spawn() error = %v, want %v", err, ErrJoinCycle)
	}
}

func TestGraphTaskSpawnAndJoin(t *testing.T) {
	t.Parallel()

	t.Run("root has no spawn or join", func(t *testing.T) {
		t.Parallel()

		root := newGraph().AddTask()
		if root.SpawnedFrom != "" {
			t.Fatalf("SpawnedFrom = %q, want empty", root.SpawnedFrom)
		}
		if got := root.Joins; !reflect.DeepEqual(got, []string{}) {
			t.Fatalf("Joins = %#v, want empty", got)
		}
		if got := root.JoinedBy; !reflect.DeepEqual(got, []string{}) {
			t.Fatalf("JoinedBy = %#v, want empty", got)
		}
	})

	t.Run("child records parent and join target", func(t *testing.T) {
		t.Parallel()

		graph := newGraph()
		root := graph.AddTask()
		child := mustSpawn(t, graph, root.Executor.ID, root.Verifier.ID)

		if child.SpawnedFrom != root.ID {
			t.Fatalf("child.SpawnedFrom = %q, want %q", child.SpawnedFrom, root.ID)
		}
		if !reflect.DeepEqual(child.Joins, []string{root.ID}) {
			t.Fatalf("child.Joins = %v, want [%s]", child.Joins, root.ID)
		}

		parent, ok := graph.Task(root.ID)
		if !ok {
			t.Fatal("parent missing")
		}
		if !reflect.DeepEqual(parent.JoinedBy, []string{child.ID}) {
			t.Fatalf("parent.JoinedBy = %v, want [%s]", parent.JoinedBy, child.ID)
		}
	})

	t.Run("join target differs from spawn parent", func(t *testing.T) {
		t.Parallel()

		graph := newGraph()
		root := graph.AddTask()
		child := mustSpawn(t, graph, root.Planner.ID, root.Executor.ID)
		grand := mustSpawn(t, graph, child.Executor.ID, root.Verifier.ID)

		if grand.SpawnedFrom != child.ID {
			t.Fatalf("grand.SpawnedFrom = %q, want %q", grand.SpawnedFrom, child.ID)
		}
		if !reflect.DeepEqual(grand.Joins, []string{root.ID}) {
			t.Fatalf("grand.Joins = %v, want [%s]", grand.Joins, root.ID)
		}

		mid, ok := graph.Task(child.ID)
		if !ok {
			t.Fatal("child missing")
		}
		if got := mid.JoinedBy; !reflect.DeepEqual(got, []string{}) {
			t.Fatalf("child.JoinedBy = %#v, want empty", got)
		}

		parent, ok := graph.Task(root.ID)
		if !ok {
			t.Fatal("parent missing")
		}
		if !reflect.DeepEqual(parent.JoinedBy, []string{child.ID, grand.ID}) {
			t.Fatalf("parent.JoinedBy = %v, want [%s %s]", parent.JoinedBy, child.ID, grand.ID)
		}
	})

	t.Run("two children join same task", func(t *testing.T) {
		t.Parallel()

		graph := newGraph()
		root := graph.AddTask()
		first := mustSpawn(t, graph, root.Planner.ID, root.Verifier.ID)
		second := mustSpawn(t, graph, root.Executor.ID, root.Verifier.ID)

		parent, ok := graph.Task(root.ID)
		if !ok {
			t.Fatal("parent missing")
		}
		if !reflect.DeepEqual(parent.JoinedBy, []string{first.ID, second.ID}) {
			t.Fatalf("parent.JoinedBy = %v, want [%s %s]", parent.JoinedBy, first.ID, second.ID)
		}
	})
}

func TestGraphTaskEnv(t *testing.T) {
	t.Parallel()

	t.Run("root env has id and no parent", func(t *testing.T) {
		t.Parallel()

		root := newGraph().AddTask()
		if root.Env.ID == "" {
			t.Fatal("root env id is empty")
		}
		if root.Env.ParentID != "" {
			t.Fatalf("root env parent = %q, want empty", root.Env.ParentID)
		}
	})

	t.Run("spawn forks parent env", func(t *testing.T) {
		t.Parallel()

		graph := newGraph()
		root := graph.AddTask()
		child := mustSpawn(t, graph, root.Executor.ID, root.Verifier.ID)

		if child.Env.ID == "" || child.Env.ID == root.Env.ID {
			t.Fatalf("child env = %#v, want new id forked from %q", child.Env, root.Env.ID)
		}
		if child.Env.ParentID != root.Env.ID {
			t.Fatalf("child env parent = %q, want %q", child.Env.ParentID, root.Env.ID)
		}

		parent, ok := graph.Task(root.ID)
		if !ok {
			t.Fatal("parent missing")
		}
		if parent.Env != root.Env {
			t.Fatalf("parent env changed after spawn: %#v", parent.Env)
		}
		if got := graph.Forks(root.Env.ID); !reflect.DeepEqual(got, []Env{child.Env}) {
			t.Fatalf("Forks() = %#v, want [%#v]", got, child.Env)
		}
		if got := graph.Impact(root.ID); !reflect.DeepEqual(got, []Env{child.Env}) {
			t.Fatalf("Impact() = %#v, want [%#v]", got, child.Env)
		}
	})

	t.Run("siblings fork the same parent and stay isolated", func(t *testing.T) {
		t.Parallel()

		graph := newGraph()
		root := graph.AddTask()
		first := mustSpawn(t, graph, root.Planner.ID, root.Verifier.ID)
		second := mustSpawn(t, graph, root.Executor.ID, root.Verifier.ID)

		if first.Env.ID == second.Env.ID {
			t.Fatal("siblings share an env id")
		}
		if first.Env.ParentID != root.Env.ID || second.Env.ParentID != root.Env.ID {
			t.Fatalf("sibling parents = %q, %q, want %q", first.Env.ParentID, second.Env.ParentID, root.Env.ID)
		}
		if got := graph.Forks(root.Env.ID); !reflect.DeepEqual(got, []Env{first.Env, second.Env}) {
			t.Fatalf("Forks() = %#v, want both siblings", got)
		}
		if got := graph.Impact(root.ID); !reflect.DeepEqual(got, []Env{first.Env, second.Env}) {
			t.Fatalf("Impact() = %#v, want both siblings", got)
		}
	})

	t.Run("nested fork follows spawn, impact follows join", func(t *testing.T) {
		t.Parallel()

		graph := newGraph()
		root := graph.AddTask()
		child := mustSpawn(t, graph, root.Planner.ID, root.Executor.ID)
		grand := mustSpawn(t, graph, child.Executor.ID, root.Verifier.ID)

		if grand.Env.ParentID != child.Env.ID {
			t.Fatalf("grand env parent = %q, want %q", grand.Env.ParentID, child.Env.ID)
		}
		if got := graph.Forks(child.Env.ID); !reflect.DeepEqual(got, []Env{grand.Env}) {
			t.Fatalf("Forks(child) = %#v, want [%#v]", got, grand.Env)
		}
		if got := graph.Impact(child.ID); !reflect.DeepEqual(got, []Env{}) {
			t.Fatalf("Impact(child) = %#v, want empty", got)
		}
		if got := graph.Impact(root.ID); !reflect.DeepEqual(got, []Env{child.Env, grand.Env}) {
			t.Fatalf("Impact(root) = %#v, want child and grand", got)
		}
	})
}

func TestGraphSpawnedTasks(t *testing.T) {
	t.Parallel()

	graph := newGraph()
	root := graph.AddTask()
	first := mustSpawn(t, graph, root.Executor.ID, root.Verifier.ID)
	second := mustSpawn(t, graph, root.Executor.ID, root.Verifier.ID)
	nested := mustSpawn(t, graph, first.Planner.ID, first.Verifier.ID)

	got := taskIDs(graph.SpawnedTasks(root.Executor.ID))
	if !reflect.DeepEqual(got, []string{first.ID, second.ID}) {
		t.Fatalf("SpawnedTasks(executor) = %v, want [%s %s]", got, first.ID, second.ID)
	}
	if got := graph.SpawnedTasks(root.Planner.ID); len(got) != 0 {
		t.Fatalf("SpawnedTasks(planner) = %#v, want empty", got)
	}
	if got := taskIDs(graph.SpawnedTasks(first.Planner.ID)); !reflect.DeepEqual(got, []string{nested.ID}) {
		t.Fatalf("SpawnedTasks(child planner) = %v, want [%s]", got, nested.ID)
	}
	if got := graph.SpawnedTasks("missing"); len(got) != 0 {
		t.Fatalf("SpawnedTasks(missing) = %#v, want empty", got)
	}
}

func TestGraphIncoming(t *testing.T) {
	t.Parallel()

	graph := newGraph()
	root := graph.AddTask()
	child := mustSpawn(t, graph, root.Executor.ID, root.Verifier.ID)

	if got := graph.Incoming(root.Planner.ID); len(got) != 0 {
		t.Fatalf("Incoming(root planner) = %#v, want empty", got)
	}
	if got := nodeIDs(graph.Incoming(root.Executor.ID)); !reflect.DeepEqual(got, []string{root.Planner.ID}) {
		t.Fatalf("Incoming(root executor) = %v, want [%s]", got, root.Planner.ID)
	}
	if got := nodeIDs(graph.Incoming(root.Verifier.ID)); !reflect.DeepEqual(got, []string{root.Executor.ID, child.Verifier.ID}) {
		t.Fatalf("Incoming(root verifier) = %v, want [%s %s]", got, root.Executor.ID, child.Verifier.ID)
	}
	if got := nodeIDs(graph.Incoming(child.Planner.ID)); !reflect.DeepEqual(got, []string{root.Executor.ID}) {
		t.Fatalf("Incoming(child planner) = %v, want [%s]", got, root.Executor.ID)
	}
	if got := graph.Incoming("missing"); len(got) != 0 {
		t.Fatalf("Incoming(missing) = %#v, want empty", got)
	}
}

func TestGraphIncomingJoins(t *testing.T) {
	t.Parallel()

	graph := newGraph()
	root := graph.AddTask()
	child := mustSpawn(t, graph, root.Executor.ID, root.Verifier.ID)

	if got := graph.IncomingJoins(root.Planner.ID); len(got) != 0 {
		t.Fatalf("IncomingJoins(root planner) = %#v, want empty", got)
	}
	if got := graph.IncomingJoins(root.Executor.ID); len(got) != 0 {
		t.Fatalf("IncomingJoins(root executor) = %#v, want empty", got)
	}
	if got := nodeIDs(graph.IncomingJoins(root.Verifier.ID)); !reflect.DeepEqual(got, []string{child.Verifier.ID}) {
		t.Fatalf("IncomingJoins(root verifier) = %v, want [%s]", got, child.Verifier.ID)
	}
	if got := graph.IncomingJoins(child.Planner.ID); len(got) != 0 {
		t.Fatalf("IncomingJoins(child planner) = %#v, want empty", got)
	}
	if got := graph.IncomingJoins("missing"); len(got) != 0 {
		t.Fatalf("IncomingJoins(missing) = %#v, want empty", got)
	}

	other := newGraph()
	parent := other.AddTask()
	nested := mustSpawn(t, other, parent.Planner.ID, parent.Executor.ID)
	if got := nodeIDs(other.IncomingJoins(parent.Executor.ID)); !reflect.DeepEqual(got, []string{nested.Verifier.ID}) {
		t.Fatalf("IncomingJoins(join target) = %v, want [%s]", got, nested.Verifier.ID)
	}
	if got := other.IncomingJoins(parent.Verifier.ID); len(got) != 0 {
		t.Fatalf("IncomingJoins(non-join verifier) = %#v, want empty", got)
	}
}

func TestExampleGraphs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		build    func(*testing.T, *Graph)
		expected int
	}{
		{
			name: "single task",
			build: func(_ *testing.T, graph *Graph) {
				graph.AddTask()
			},
			expected: 1,
		},
		{
			name: "spawn from executor join verifier",
			build: func(t *testing.T, graph *Graph) {
				root := graph.AddTask()
				mustSpawn(t, graph, root.Executor.ID, root.Verifier.ID)
			},
			expected: 2,
		},
		{
			name: "planner fork then nested into verifier",
			build: func(t *testing.T, graph *Graph) {
				root := graph.AddTask()
				child := mustSpawn(t, graph, root.Planner.ID, root.Executor.ID)
				mustSpawn(t, graph, child.Executor.ID, root.Verifier.ID)
			},
			expected: 3,
		},
		{
			name: "two children join verifier",
			build: func(t *testing.T, graph *Graph) {
				root := graph.AddTask()
				mustSpawn(t, graph, root.Planner.ID, root.Verifier.ID)
				mustSpawn(t, graph, root.Executor.ID, root.Verifier.ID)
			},
			expected: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			graph := newGraph()
			tt.build(t, graph)
			if got := graph.taskCount(); got != tt.expected {
				t.Fatalf("tasks = %d, want %d", got, tt.expected)
			}
			t.Logf("\n%s", dumpMermaid(graph))
		})
	}
}

func resetDefault(t *testing.T) {
	t.Helper()
	Default().reset()
	t.Cleanup(func() { Default().reset() })
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

func mustSpawn(t *testing.T, graph *Graph, from, join string) Task {
	t.Helper()
	task, err := graph.Spawn(from, join)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func (g *Graph) taskCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.tasks)
}

func dumpMermaid(g *Graph) string {
	g.mu.Lock()
	defer g.mu.Unlock()

	var b strings.Builder
	b.WriteString("flowchart LR\n")
	for _, task := range g.tasks {
		fmt.Fprintf(&b, "  subgraph %s[\"%s\"]\n", mermaidID(task.ID), task.ID)
		for _, node := range task.Sequence() {
			fmt.Fprintf(&b, "    %s[\"%s\"]\n", mermaidID(node.ID), node.Role)
		}
		b.WriteString("  end\n")
	}
	for _, edge := range g.edges {
		arrow := "-->"
		if edge.Kind != EdgeKindSequence {
			arrow = "-.->|" + edge.Kind + "|"
		}
		fmt.Fprintf(&b, "  %s %s %s\n", mermaidID(edge.From), arrow, mermaidID(edge.To))
	}
	return b.String()
}

func mermaidID(id string) string {
	return strings.NewReplacer("-", "_", ":", "_").Replace(id)
}
