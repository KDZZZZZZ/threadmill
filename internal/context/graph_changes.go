package context

import (
	"fmt"
	"strings"
)

// NodeChange 的 Action 取值。
const (
	NodeChangeCreate = "create"
	NodeChangeUpdate = "update"
	NodeChangeStatus = "status"
	NodeChangeDelete = "delete"
	NodeChangeAttach = "attach"
	NodeChangeDetach = "detach"
)

// NodeChange 描述一次对记忆节点的变更；由 WithNodeChanges 按序原子应用。
type NodeChange struct {
	Action         string   // create、update、status、delete、attach 或 detach
	ID             string   // 目标节点 ID；create 时可留空由系统分配
	Kind           string   // create 必填；update 可选
	Statement      string   // create/update 必填，update 为整条替换
	Status         string   // status 必填；create/update 可选
	SupersededBy   string   // status=superseded 时的取代者节点 ID
	SubgraphIDs    []string // create/attach/detach 的目标子图
	CreatorAgentID string   // create 时的创建者
}

// WithNodeChanges 按序校验并应用一批节点变更；任何一条非法则返回错误且不产生新图。
// 变更在克隆上执行，全部成功才返回；有空子图归属的 create/attach 沿用既有约定（只保留已有子图）。
func (g Graph) WithNodeChanges(changes []NodeChange) (Graph, error) {
	out := g.Clone()
	if len(changes) == 0 {
		return out, nil
	}
	changed := false
	for i, change := range changes {
		var err error
		switch change.Action {
		case NodeChangeCreate:
			err = applyNodeCreate(&out, change)
		case NodeChangeUpdate:
			err = applyNodeUpdate(&out, change)
		case NodeChangeStatus:
			err = applyNodeStatus(&out, change)
		case NodeChangeDelete:
			err = applyNodeDelete(&out, change)
		case NodeChangeAttach, NodeChangeDetach:
			err = applyNodeMembership(&out, change)
		default:
			err = fmt.Errorf("unknown action %q", change.Action)
		}
		if err != nil {
			return Graph{}, fmt.Errorf("context: node change %d: %w", i+1, err)
		}
		changed = true
	}
	if changed {
		out.Revision++
	}
	return out, nil
}

func applyNodeCreate(out *Graph, change NodeChange) error {
	if strings.TrimSpace(change.Statement) == "" {
		return fmt.Errorf("create: statement is required")
	}
	if !validNodeKind(change.Kind) {
		return fmt.Errorf("create: invalid kind %q", change.Kind)
	}
	if change.Status != "" && !validNodeStatus(change.Status) {
		return fmt.Errorf("create: invalid status %q", change.Status)
	}
	if change.ID != "" {
		if _, exists := out.nodeByID(change.ID); exists {
			return fmt.Errorf("create: node ID %q already exists", change.ID)
		}
	} else {
		change.ID = nextMemNodeID(*out)
	}
	if change.SupersededBy != "" {
		if _, exists := out.nodeByID(change.SupersededBy); !exists {
			return fmt.Errorf("create: superseded_by %q not found", change.SupersededBy)
		}
	}
	node := Node{
		ID:             change.ID,
		Kind:           change.Kind,
		Statement:      strings.TrimSpace(change.Statement),
		Status:         change.Status,
		SubgraphIDs:    keepKnownIDs(change.SubgraphIDs, subgraphIDs(*out)),
		CreatorAgentID: change.CreatorAgentID,
	}
	if node.Status == "" {
		node.Status = NodeStatusAccepted
	}
	next := out.WithMemory([]Node{node}, nil)
	*out = next
	return nil
}

func applyNodeUpdate(out *Graph, change NodeChange) error {
	index, ok := out.nodeIndex(change.ID)
	if !ok {
		return fmt.Errorf("update: node %q not found", change.ID)
	}
	if strings.TrimSpace(change.Statement) == "" {
		return fmt.Errorf("update: statement is required for full replacement")
	}
	if change.Kind != "" && !validNodeKind(change.Kind) {
		return fmt.Errorf("update: invalid kind %q", change.Kind)
	}
	if change.Status != "" && !validNodeStatus(change.Status) {
		return fmt.Errorf("update: invalid status %q", change.Status)
	}
	if change.SupersededBy != "" {
		if _, exists := out.nodeByID(change.SupersededBy); !exists {
			return fmt.Errorf("update: superseded_by %q not found", change.SupersededBy)
		}
	}
	node := cloneNode(out.Nodes[index])
	node.Statement = strings.TrimSpace(change.Statement)
	if change.Kind != "" {
		node.Kind = change.Kind
	}
	if change.Status != "" {
		node.Status = change.Status
	}
	if change.SupersededBy != "" {
		node.SupersededBy = change.SupersededBy
	}
	out.Nodes[index] = node
	return nil
}

func applyNodeStatus(out *Graph, change NodeChange) error {
	index, ok := out.nodeIndex(change.ID)
	if !ok {
		return fmt.Errorf("status: node %q not found", change.ID)
	}
	if !validNodeStatus(change.Status) {
		return fmt.Errorf("status: invalid status %q", change.Status)
	}
	if change.SupersededBy != "" {
		if change.SupersededBy == change.ID {
			return fmt.Errorf("status: node %q cannot supersede itself", change.ID)
		}
		if _, exists := out.nodeByID(change.SupersededBy); !exists {
			return fmt.Errorf("status: superseded_by %q not found", change.SupersededBy)
		}
	}
	node := cloneNode(out.Nodes[index])
	node.Status = change.Status
	if change.Status == NodeStatusSuperseded {
		node.SupersededBy = change.SupersededBy
	} else {
		node.SupersededBy = ""
	}
	out.Nodes[index] = node
	return nil
}

func applyNodeDelete(out *Graph, change NodeChange) error {
	index, ok := out.nodeIndex(change.ID)
	if !ok {
		return fmt.Errorf("delete: node %q not found", change.ID)
	}
	removed := out.Nodes[index].ID
	nodes := make([]Node, 0, len(out.Nodes)-1)
	for _, node := range out.Nodes {
		if node.ID != removed {
			nodes = append(nodes, node)
		}
	}
	edges := make([]Edge, 0, len(out.Edges))
	for _, edge := range out.Edges {
		if edge.ToNodeID == removed {
			continue
		}
		if id, ok := parseRef(edge.FromRef, nodeRefPrefix); ok && id == removed {
			continue
		}
		edges = append(edges, edge)
	}
	out.Nodes = nodes
	out.Edges = edges
	return nil
}

func applyNodeMembership(out *Graph, change NodeChange) error {
	index, ok := out.nodeIndex(change.ID)
	if !ok {
		return fmt.Errorf("%s: node %q not found", change.Action, change.ID)
	}
	ids := keepKnownIDs(change.SubgraphIDs, subgraphIDs(*out))
	if len(ids) == 0 {
		return fmt.Errorf("%s: no known subgraph ids", change.Action)
	}
	node := cloneNode(out.Nodes[index])
	membership := node.SubgraphIDs
	if change.Action == NodeChangeAttach {
		membership = unionIDs(membership, ids)
		if !hasSubgraphID(*out, ids[len(ids)-1]) {
			out.Subgraphs = append(out.Subgraphs, Subgraph{ID: ids[len(ids)-1]})
		}
	} else {
		kept := make([]string, 0, len(membership))
		for _, id := range membership {
			if containsID(ids, id) {
				continue
			}
			kept = append(kept, id)
		}
		membership = kept
	}
	node.SubgraphIDs = membership
	out.Nodes[index] = node
	return nil
}

func validNodeKind(kind string) bool {
	switch kind {
	case NodeKindDirective, NodeKindFact, NodeKindHypothesis:
		return true
	default:
		return false
	}
}

func validNodeStatus(status string) bool {
	switch status {
	case NodeStatusAccepted, NodeStatusDisputed, NodeStatusSuperseded, NodeStatusOutdated:
		return true
	default:
		return false
	}
}

func (g Graph) nodeIndex(id string) (int, bool) {
	if id == "" {
		return 0, false
	}
	for i, node := range g.Nodes {
		if node.ID == id {
			return i, true
		}
	}
	return 0, false
}

func nextMemNodeID(g Graph) string {
	used := make(map[string]struct{}, len(g.Nodes))
	for _, node := range g.Nodes {
		if node.ID != "" {
			used[node.ID] = struct{}{}
		}
	}
	for i := 1; ; i++ {
		id := fmt.Sprintf("mem-%d", i)
		if _, taken := used[id]; taken {
			continue
		}
		return id
	}
}
