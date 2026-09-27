package runtime

import (
	"fmt"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/compiler"
)

func TestNewSceneConsumersDeduplicateSmallAndWideFanIn(t *testing.T) {
	tests := []struct {
		name  string
		fanIn int
	}{
		{name: "small", fanIn: 8},
		{name: "wide", fanIn: 9},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodes := make([]compiler.GraphNode, 0, tt.fanIn+1)
			upstream := make([]string, 0, tt.fanIn)
			expectedPaths := make(map[string]struct{}, tt.fanIn)
			for i := 0; i < tt.fanIn; i++ {
				id := fmt.Sprintf("source-%d", i)
				path := fmt.Sprintf("source.path.%d", i)
				if i == 1 {
					path = "source.path.0"
				}
				nodes = append(nodes, compiler.GraphNode{ID: id, Kind: "input", Path: path})
				upstream = append(upstream, id)
				expectedPaths[path] = struct{}{}
			}
			nodes = append(nodes, compiler.GraphNode{
				ID:       "computed",
				Kind:     "computed",
				Compute:  "missing.test.compute@1",
				Upstream: upstream,
			})

			graph := &compiler.Graph{
				SceneID: "scene-index-" + tt.name,
				Nodes:   nodes,
			}
			scene := NewScene(graph.SceneID, graph, &compiler.RenderBundle{}, NewComputeRegistry(), quietLogger())
			defer scene.cancel()

			computedIndex := tt.fanIn
			for path := range expectedPaths {
				consumers := scene.consumers[path]
				if len(consumers) != 1 || consumers[0] != computedIndex {
					t.Errorf("consumers[%q] = %v, want exactly [%d]", path, consumers, computedIndex)
				}
			}
			if len(scene.consumers) != len(expectedPaths) {
				t.Errorf("consumer path count = %d, want %d", len(scene.consumers), len(expectedPaths))
			}
		})
	}
}

func TestComputeEntryUsesNamedInputsWithLegacyFallback(t *testing.T) {
	tests := []struct {
		name string
		node compiler.GraphNode
		want []string
	}{
		{
			name: "named inputs are authoritative",
			node: compiler.GraphNode{
				Upstream: []string{"legacy.source"},
				Inputs:   []compiler.GraphInput{{From: "named.source", Port: "value"}},
			},
			want: []string{"named.source"},
		},
		{
			name: "legacy upstream fallback",
			node: compiler.GraphNode{Upstream: []string{"legacy.source"}},
			want: []string{"legacy.source"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry := computeEntry{node: tt.node}
			if got := entry.upstreamCount(); got != len(tt.want) {
				t.Fatalf("upstreamCount() = %d, want %d", got, len(tt.want))
			}
			for i, want := range tt.want {
				if got := entry.upstreamID(i); got != want {
					t.Errorf("upstreamID(%d) = %q, want %q", i, got, want)
				}
			}
		})
	}
}

// BenchmarkNewScene20kColdStart isolates runtime loading and cold evaluation
// from compiler cost so runtime initialization changes can be compared directly.
func BenchmarkNewScene20kColdStart(b *testing.B) {
	graph := compile20k(b)
	bundle := &compiler.RenderBundle{SceneVersion: graph.SceneVersion}
	registry := NewComputeRegistry()
	logger := quietLogger()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		scene := NewScene("bench-20k", graph, bundle, registry, logger)
		scene.cancel()
	}
}

// BenchmarkNewScene20kInputOnly guards the sparse-graph case: preallocation
// must not turn an empty reverse-adjacency index into a large allocation.
func BenchmarkNewScene20kInputOnly(b *testing.B) {
	const nodeCount = 20_000
	nodes := make([]compiler.GraphNode, nodeCount)
	for i := range nodes {
		nodes[i] = compiler.GraphNode{ID: fmt.Sprintf("input-%d", i), Kind: "input"}
	}
	graph := &compiler.Graph{SceneID: "bench-20k-input-only", Nodes: nodes}
	bundle := &compiler.RenderBundle{SceneVersion: "sha256:input-only"}
	registry := NewComputeRegistry()
	logger := quietLogger()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		scene := NewScene(graph.SceneID, graph, bundle, registry, logger)
		scene.cancel()
	}
}
