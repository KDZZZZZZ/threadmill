package context

import (
	"encoding/json"
	"fmt"
	"slices"
)

// InputSource is one fixed predecessor output, including its provenance.
type InputSource struct {
	Ref   string `json:"ref"`
	Graph Graph  `json:"graph"`
}

// InputPartition separates the intersection from each source's remaining values.
// Its component graphs need not be closed: common edges may refer to differing nodes.
type InputPartition struct {
	Common      Graph         `json:"common"`
	Differences []InputSource `json:"differences"`
}

// PartitionInputs compares complete source states, independent of historical baselines.
func PartitionInputs(sources []InputSource) InputPartition {
	if len(sources) == 0 {
		return InputPartition{}
	}
	if len(sources) == 1 {
		return InputPartition{Common: sources[0].Graph.Clone()}
	}
	nodes := make([][]Node, len(sources))
	subgraphs := make([][]Subgraph, len(sources))
	edges := make([][]Edge, len(sources))
	result := InputPartition{Differences: make([]InputSource, len(sources))}
	for i, source := range sources {
		graph := source.Graph.Clone()
		nodes[i], subgraphs[i], edges[i] = graph.Nodes, graph.Subgraphs, graph.Edges
		result.Common.Revision = max(result.Common.Revision, graph.Revision)
		result.Differences[i] = InputSource{Ref: source.Ref, Graph: Graph{Revision: graph.Revision}}
	}
	commonNodes, differingNodes := partitionValues(nodes, inputNodeKey)
	commonSubgraphs, differingSubgraphs := partitionValues(subgraphs, inputSubgraphKey)
	commonEdges, differingEdges := partitionValues(edges, inputJSON[Edge])
	result.Common.Nodes, result.Common.Subgraphs, result.Common.Edges = commonNodes, commonSubgraphs, commonEdges
	for i := range sources {
		result.Differences[i].Graph.Nodes = differingNodes[i]
		result.Differences[i].Graph.Subgraphs = differingSubgraphs[i]
		result.Differences[i].Graph.Edges = differingEdges[i]
	}
	return result
}

// HasDifferences reports whether any source has values outside the intersection.
func (p InputPartition) HasDifferences() bool {
	for _, source := range p.Differences {
		if len(source.Graph.Nodes)+len(source.Graph.Subgraphs)+len(source.Graph.Edges) > 0 {
			return true
		}
	}
	return false
}

// Draft retains every differing version with its source reference. Colliding IDs
// are remapped together with memberships and relationships; common values stay intact.
// The returned graph is a candidate, not accepted target memory.
func (p InputPartition) Draft() (Graph, error) {
	out := p.Common.Clone()
	usedNodes, usedSubgraphs := make(map[string]bool), make(map[string]bool)
	for _, source := range append([]InputSource{{Graph: p.Common}}, p.Differences...) {
		for _, node := range source.Graph.Nodes {
			usedNodes[node.ID] = true
		}
		for _, subgraph := range source.Graph.Subgraphs {
			usedSubgraphs[subgraph.ID] = true
		}
	}
	nodeVersions, subgraphVersions := make(map[string]string), make(map[string]string)
	for _, source := range p.Differences {
		nodeIDs, subgraphIDs := make(map[string]string), make(map[string]string)
		for _, subgraph := range source.Graph.Subgraphs {
			originalID := subgraph.ID
			key := inputSubgraphKey(subgraph)
			id := subgraphVersions[key]
			if id == "" {
				id = subgraph.ID
				if hasSubgraphID(out, id) {
					id = inputVariantID(id, usedSubgraphs)
				}
				subgraphVersions[key] = id
				subgraph.ID = id
				out.Subgraphs = append(out.Subgraphs, subgraph)
			}
			subgraphIDs[originalID] = id
		}
		for _, node := range source.Graph.Nodes {
			key := inputNodeKey(node)
			id := nodeVersions[key]
			if id == "" {
				id = node.ID
				if _, exists := out.nodeByID(id); exists {
					id = inputVariantID(id, usedNodes)
				}
				nodeVersions[key] = id
				placeholder := cloneNode(node)
				placeholder.ID = id
				placeholder.SubgraphIDs = nil
				out.Nodes = append(out.Nodes, placeholder)
			}
			nodeIDs[node.ID] = id
		}
		for _, node := range source.Graph.Nodes {
			index, _ := out.nodeIndex(nodeIDs[node.ID])
			for _, id := range node.SubgraphIDs {
				out.Nodes[index].SubgraphIDs = unionIDs(out.Nodes[index].SubgraphIDs, []string{inputMappedID(id, subgraphIDs)})
			}
			out.Nodes[index].SupersededBy = inputMappedID(node.SupersededBy, nodeIDs)
			out.Nodes[index].SourceRefs = unionIDs(out.Nodes[index].SourceRefs, []string{source.Ref})
		}
		for _, edge := range append(append([]Edge(nil), p.Common.Edges...), source.Graph.Edges...) {
			edge = rewriteEdge(edge, nodeIDs)
			if id, ok := parseRef(edge.FromRef, subgraphRefPrefix); ok {
				edge.FromRef = SubgraphRef(inputMappedID(id, subgraphIDs))
			}
			if !hasEdge(out, edge) {
				out.Edges = append(out.Edges, edge)
			}
		}
	}
	if err := out.ValidateReferences(); err != nil {
		return Graph{}, err
	}
	return out, nil
}

// Compose combines processed differences with common memory and validates the
// complete graph. A common node can only be superseded by an explicit correction;
// its original statement, provenance and memberships cannot be rewritten.
func (p InputPartition) Compose(processed Graph) (Graph, error) {
	if _, _, err := inputIdentities(processed); err != nil {
		return Graph{}, err
	}
	out := p.Common.Clone()
	for _, subgraph := range processed.Subgraphs {
		found := false
		for _, common := range p.Common.Subgraphs {
			if common.ID == subgraph.ID {
				if inputSubgraphKey(common) != inputSubgraphKey(subgraph) {
					return Graph{}, fmt.Errorf("context: common subgraph %q is read-only", subgraph.ID)
				}
				found = true
				break
			}
		}
		if !found {
			out.Subgraphs = append(out.Subgraphs, subgraph)
		}
	}
	for _, node := range processed.Nodes {
		index, common := p.Common.nodeIndex(node.ID)
		if !common {
			out.Nodes = append(out.Nodes, cloneNode(node))
			continue
		}
		original := p.Common.Nodes[index]
		comparison := cloneNode(node)
		comparison.Status, comparison.SupersededBy = original.Status, original.SupersededBy
		if inputNodeKey(comparison) != inputNodeKey(original) {
			return Graph{}, fmt.Errorf("context: common node %q is read-only", node.ID)
		}
		if node.Status != original.Status || node.SupersededBy != original.SupersededBy {
			_, correctionIsCommon := p.Common.nodeByID(node.SupersededBy)
			correction, exists := processed.nodeByID(node.SupersededBy)
			protected := original.Kind == NodeKindDirective || original.CreatorAgentID == "user" || original.CreatorAgentID == "system"
			validCorrection := exists && correction.Status == NodeStatusAccepted && !correctionIsCommon
			if protected || node.Status != NodeStatusSuperseded || !validCorrection {
				return Graph{}, fmt.Errorf("context: common node %q requires a differing correction", node.ID)
			}
			out.Nodes[index] = cloneNode(node)
		}
	}
	for _, edge := range processed.Edges {
		if !hasEdge(out, edge) {
			out.Edges = append(out.Edges, edge)
		}
	}
	out.Revision = max(p.Common.Revision, processed.Revision) + 1
	if err := out.ValidateReferences(); err != nil {
		return Graph{}, err
	}
	return out, nil
}

// ValidateReferences rejects duplicate identities and dangling graph references.
// SourceRefs can refer to historical outputs outside this graph and are not endpoints.
func (g Graph) ValidateReferences() error {
	nodes, subgraphs, err := inputIdentities(g)
	if err != nil {
		return err
	}
	for _, node := range g.Nodes {
		for _, id := range node.SubgraphIDs {
			if !subgraphs[id] {
				return fmt.Errorf("context: node %q has unknown subgraph %q", node.ID, id)
			}
		}
		if node.SupersededBy != "" && (!nodes[node.SupersededBy] || node.SupersededBy == node.ID) {
			return fmt.Errorf("context: node %q has invalid superseded_by %q", node.ID, node.SupersededBy)
		}
	}
	for _, edge := range g.Edges {
		validFrom := false
		if id, ok := parseRef(edge.FromRef, nodeRefPrefix); ok {
			validFrom = nodes[id]
		}
		if id, ok := parseRef(edge.FromRef, subgraphRefPrefix); ok {
			validFrom = subgraphs[id]
		}
		if !validFrom || !nodes[edge.ToNodeID] {
			return fmt.Errorf("context: dangling edge %q -> %q", edge.FromRef, edge.ToNodeID)
		}
	}
	return nil
}

func inputIdentities(g Graph) (map[string]bool, map[string]bool, error) {
	nodes, subgraphs := make(map[string]bool), make(map[string]bool)
	for _, node := range g.Nodes {
		if node.ID == "" || nodes[node.ID] {
			return nil, nil, fmt.Errorf("context: empty or duplicate node ID %q", node.ID)
		}
		nodes[node.ID] = true
	}
	for _, subgraph := range g.Subgraphs {
		if subgraph.ID == "" || subgraphs[subgraph.ID] {
			return nil, nil, fmt.Errorf("context: empty or duplicate subgraph ID %q", subgraph.ID)
		}
		subgraphs[subgraph.ID] = true
	}
	return nodes, subgraphs, nil
}

func inputVariantID(id string, used map[string]bool) string {
	for n := 1; ; n++ {
		candidate := fmt.Sprintf("%s@input-%d", id, n)
		if !used[candidate] {
			used[candidate] = true
			return candidate
		}
	}
}

func inputMappedID(id string, remap map[string]string) string {
	if mapped, ok := remap[id]; ok {
		return mapped
	}
	return id
}

func partitionValues[T any](sources [][]T, key func(T) string) ([]T, [][]T) {
	counts := make(map[string]int)
	for _, values := range sources {
		seen := make(map[string]bool, len(values))
		for _, value := range values {
			k := key(value)
			if !seen[k] {
				counts[k]++
				seen[k] = true
			}
		}
	}
	var common []T
	differences := make([][]T, len(sources))
	for i, values := range sources {
		seen := make(map[string]bool, len(values))
		for _, value := range values {
			k := key(value)
			if seen[k] {
				continue
			}
			seen[k] = true
			if counts[k] == len(sources) {
				if i == 0 {
					common = append(common, value)
				}
			} else {
				differences[i] = append(differences[i], value)
			}
		}
	}
	return common, differences
}

func inputNodeKey(node Node) string {
	node.SubgraphIDs = sortedInputIDs(node.SubgraphIDs)
	node.SourceRefs = sortedInputIDs(node.SourceRefs)
	return inputJSON(node)
}

func inputSubgraphKey(subgraph Subgraph) string {
	subgraph.Revision = 0
	return inputJSON(subgraph)
}

func sortedInputIDs(ids []string) []string {
	result := append([]string{}, ids...)
	slices.Sort(result)
	return slices.Compact(result)
}

// Input values contain only strings, integers and slices, all JSON-encodable.
func inputJSON[T any](value T) string {
	data, _ := json.Marshal(value)
	return string(data)
}
