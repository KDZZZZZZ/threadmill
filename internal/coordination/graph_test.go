package coordination

import (
	"errors"
	"reflect"
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

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
