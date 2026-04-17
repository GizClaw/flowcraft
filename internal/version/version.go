// Package version provides graph version management with draft/publish/rollback
// lifecycle, checksum-based optimistic locking, and diff computation.
package version

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/GizClaw/flowcraft/internal/model"
)

// GraphDiff describes the differences between two graph versions.
type GraphDiff struct {
	FromVersion  int                    `json:"from_version"`
	ToVersion    int                    `json:"to_version"`
	NodesAdded   []model.NodeDefinition `json:"nodes_added,omitempty"`
	NodesRemoved []model.NodeDefinition `json:"nodes_removed,omitempty"`
	NodesChanged []NodeChange           `json:"nodes_changed,omitempty"`
	EdgesAdded   []model.EdgeDefinition `json:"edges_added,omitempty"`
	EdgesRemoved []model.EdgeDefinition `json:"edges_removed,omitempty"`
}

// NodeChange records modifications to a single node between versions.
type NodeChange struct {
	NodeID string         `json:"node_id"`
	Before map[string]any `json:"before"`
	After  map[string]any `json:"after"`
}

// ComputeChecksum returns a SHA256 hex digest of a serialized GraphDefinition.
func ComputeChecksum(def *model.GraphDefinition) string {
	data, _ := json.Marshal(def)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

// computeDiff calculates the difference between two GraphDefinitions.
func computeDiff(from, to *model.GraphDefinition, fromVer, toVer int) *GraphDiff {
	diff := &GraphDiff{FromVersion: fromVer, ToVersion: toVer}

	fromNodes := indexNodes(from)
	toNodes := indexNodes(to)

	for id, node := range toNodes {
		if _, exists := fromNodes[id]; !exists {
			diff.NodesAdded = append(diff.NodesAdded, node)
		}
	}
	for id, node := range fromNodes {
		if _, exists := toNodes[id]; !exists {
			diff.NodesRemoved = append(diff.NodesRemoved, node)
		}
	}
	for id, fromNode := range fromNodes {
		toNode, exists := toNodes[id]
		if !exists {
			continue
		}
		changes := diffNode(fromNode, toNode)
		if changes != nil {
			diff.NodesChanged = append(diff.NodesChanged, *changes)
		}
	}

	fromEdges := indexEdges(from)
	toEdges := indexEdges(to)

	for key, edge := range toEdges {
		if _, exists := fromEdges[key]; !exists {
			diff.EdgesAdded = append(diff.EdgesAdded, edge)
		}
	}
	for key, edge := range fromEdges {
		if _, exists := toEdges[key]; !exists {
			diff.EdgesRemoved = append(diff.EdgesRemoved, edge)
		}
	}

	return diff
}

func indexNodes(def *model.GraphDefinition) map[string]model.NodeDefinition {
	if def == nil {
		return nil
	}
	m := make(map[string]model.NodeDefinition, len(def.Nodes))
	for _, n := range def.Nodes {
		m[n.ID] = n
	}
	return m
}

func indexEdges(def *model.GraphDefinition) map[string]model.EdgeDefinition {
	if def == nil {
		return nil
	}
	m := make(map[string]model.EdgeDefinition, len(def.Edges))
	for _, e := range def.Edges {
		key := e.From + "->" + e.To
		if e.Condition != "" {
			key += "?" + e.Condition
		}
		m[key] = e
	}
	return m
}

func diffNode(a, b model.NodeDefinition) *NodeChange {
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	if string(aj) == string(bj) {
		return nil
	}
	var before, after map[string]any
	_ = json.Unmarshal(aj, &before)
	_ = json.Unmarshal(bj, &after)
	return &NodeChange{
		NodeID: a.ID,
		Before: before,
		After:  after,
	}
}
