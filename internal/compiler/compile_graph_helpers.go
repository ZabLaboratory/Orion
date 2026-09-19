package compiler

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// eventsLeafPrefix is the `__events.` namespace on-event entries listen
// to (mirrors runtime.eventsPrefix, scene.go:665). The compiler cannot
// import the runtime package (cycle), so the literal is mirrored here —
// the same discipline execEntryKind applies to the entry-kind vocabulary.
const eventsLeafPrefix = "__events."

// wiredPorts returns the set of input port names on nodeID that have an
// inbound edge (so their value comes from upstream, not a default).
func wiredPorts(nodeID string, edges []BlueprintEdge) map[string]struct{} {
	wired := make(map[string]struct{})
	for _, e := range edges {
		if e.ToNode == nodeID {
			wired[e.ToPort] = struct{}{}
		}
	}
	return wired
}

// topologicalSort returns the nodes in dependency order using Kahn's
// algorithm. A cycle in the blueprint edge graph yields an error.
func topologicalSort(nodes []GraphNode, edges []BlueprintEdge) ([]GraphNode, error) {
	inbound := make(map[string]int, len(nodes))
	byID := make(map[string]GraphNode, len(nodes))
	for _, n := range nodes {
		inbound[n.ID] = 0
		byID[n.ID] = n
	}
	out := make(map[string][]string, len(nodes))
	for _, e := range edges {
		// Skip edges referencing nodes the validator dropped.
		if _, ok := byID[e.FromNode]; !ok {
			continue
		}
		if _, ok := byID[e.ToNode]; !ok {
			continue
		}
		out[e.FromNode] = append(out[e.FromNode], e.ToNode)
		inbound[e.ToNode]++
	}

	var queue []string
	for id := range byID {
		if inbound[id] == 0 {
			queue = append(queue, id)
		}
	}
	// Stable order for hash determinism.
	sort.Strings(queue)

	var sorted []GraphNode
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		sorted = append(sorted, byID[id])
		var newReady []string
		for _, child := range out[id] {
			inbound[child]--
			if inbound[child] == 0 {
				newReady = append(newReady, child)
			}
		}
		sort.Strings(newReady)
		queue = append(queue, newReady...)
	}
	if len(sorted) != len(byID) {
		return nil, fmt.Errorf("topological sort: cycle detected (%d/%d nodes ordered)", len(sorted), len(byID))
	}
	return sorted, nil
}

// extractAdapters reads the layout's external_adapters declaration
// (Canvas authors them at scene-edit time, the compiler validates
// shape and forwards them to the bundle + graph).
//
// v1 stub: Canvas doesn't expose external_adapters in the layout
// type yet (waiting on chantier-canvas-extensions). The compiler is
// ready to read them; today we walk the bundle for any node whose
// kind starts with `adapter:` and pluck it out. This keeps the
// runtime contract intact while Canvas catches up.
func extractAdapters(layout *CanvasLayout, _ *[]OperatorInput, _ *Diagnostics) []ExternalAdapter {
	// Forward whatever the layout brought in. v1 layouts won't yet
	// carry adapter declarations; when Canvas ships its extensions,
	// CanvasLayout gets a typed field and this method matures.
	_ = layout
	return nil
}

// duplicateInputPaths returns the path values that appear more than once.
func duplicateInputPaths(inputs []OperatorInput) []string {
	seen := make(map[string]int, len(inputs))
	for _, in := range inputs {
		seen[in.Path]++
	}
	var dups []string
	for p, n := range seen {
		if n > 1 {
			dups = append(dups, p)
		}
	}
	sort.Strings(dups)
	return dups
}

// walkLayout yields every node in the tree rooted at n.
func walkLayout(n LayoutNode, fn func(LayoutNode)) {
	fn(n)
	for _, c := range n.Children {
		walkLayout(c, fn)
	}
}

// joinPath assembles a dotted instance path. Empty fragments collapse
// so root nodes don't carry a leading `.`.
func joinPath(parts ...string) string {
	var b []string
	for _, p := range parts {
		if p == "" {
			continue
		}
		b = append(b, p)
	}
	return strings.Join(b, ".")
}

// computeSceneVersion hashes both artefacts canonically. We use
// canonical JSON (keys sorted, no insignificant whitespace) so two
// pushes of identical inputs always land on the same hash.
func computeSceneVersion(graph *Graph, bundle *RenderBundle) (string, error) {
	g, err := canonicalJSON(graph)
	if err != nil {
		return "", err
	}
	b, err := canonicalJSON(bundle)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write(g)
	h.Write([]byte("|"))
	h.Write(b)
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// canonicalJSON marshals v with keys sorted recursively. encoding/json
// is already key-stable for maps but not for nested any-typed values;
// we round-trip via map[string]any to enforce sort everywhere.
func canonicalJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, err
	}
	return marshalCanonical(generic)
}

func marshalCanonical(v any) ([]byte, error) {
	buf := &bytes.Buffer{}
	if err := writeCanonical(buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeCanonical(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case map[string]any:
		buf.WriteByte('{')
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			kb, _ := json.Marshal(k)
			buf.Write(kb)
			buf.WriteByte(':')
			if err := writeCanonical(buf, t[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	case []any:
		buf.WriteByte('[')
		for i, item := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	default:
		raw, err := json.Marshal(t)
		if err != nil {
			return err
		}
		buf.Write(raw)
	}
	return nil
}
