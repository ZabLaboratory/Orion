package lsdp

import (
	"encoding/json"
	"strings"

	"github.com/ZabLaboratory/Orion/internal/compiler"
)

// boundLeafSet is the renderable surface of a scene: the set of leaf
// paths the active scene's render bundle actually binds. The LSDP wire
// emits ONLY leaves in (or under) this set — every `__vars..` compute
// intermediate (object rows, clause descriptors, empty WHERE literals
// `whereEmptyN=[]`, scalar work leaves `catA0`/`getScore0`) that no
// layout node binds is dropped at the tap, present and future, in one
// stroke. This is the definitive hygiene contract (the per-type
// isLSDPScalar filter is kept as defense-in-depth on the leaves that
// DO pass, never as the primary gate).
//
// # Why a PREFIX set, not an exact-match set
//
// A binding may target an array/object root whose RENDERABLE leaves are
// addressed by descendant paths the bundle never names statically:
//
//   - `repeat.items` binds `rows` ; the per-iteration children resolve
//     to `rows.0.name`, `rows.1.score`, … at render time (scope.tsx
//     scopedPath). The bundle only carries the `rows` binding, so an
//     exact-match allow-list would DROP every `rows.{i}.*` leaf and
//     black out the list — the very failure we are preventing.
//
// So membership is "equals a bound path OR is a dot-descendant of one".
// A leaf `p` is kept iff some bound path `b` satisfies `p == b` or
// `strings.HasPrefix(p, b+".")`.
//
// # Empty set = disabled (fail-open)
//
// A bundle with no bindings (a passthrough/operator-only scene, or a
// scene whose bundle was not threaded) yields an EMPTY set. An empty
// set DISABLES the bound-leaf gate entirely (every leaf falls through
// to the isLSDPScalar scalar filter) — never an all-drop black screen.
// This keeps the existing single-scene/passthrough wire tests and the
// bespoke /show/stream behaviour intact.
type boundLeafSet struct {
	// exact holds every bound path; lookups also test dot-descendants.
	exact map[string]struct{}
}

// active reports whether the gate is engaged (the bundle bound at least
// one leaf). When false the caller must fall back to the scalar filter.
func (b boundLeafSet) active() bool { return len(b.exact) > 0 }

// renderable reports whether `path` is a renderable leaf: it equals a
// bound path or is a dot-descendant of one. Only meaningful when
// active() is true.
func (b boundLeafSet) renderable(path string) bool {
	if _, ok := b.exact[path]; ok {
		return true
	}
	// Dot-descendant test: walk ancestor prefixes of `path` and check
	// each against the bound set. `rows.0.name` → test `rows.0`, `rows`.
	// This is O(depth) per leaf; leaf paths are shallow.
	for i := strings.LastIndexByte(path, '.'); i > 0; i = strings.LastIndexByte(path[:i], '.') {
		if _, ok := b.exact[path[:i]]; ok {
			return true
		}
	}
	return false
}

// boundLeavesFromBundle walks a render bundle and collects every leaf
// path bound by the layout: per-node `bindings` values and
// `keyframes.key`, plus operator-input paths (operator surfaces are
// renderable inputs, never compute intermediates). A nil bundle yields
// an empty (disabled) set.
func boundLeavesFromBundle(bundle *compiler.RenderBundle) boundLeafSet {
	set := boundLeafSet{exact: map[string]struct{}{}}
	if bundle == nil {
		return set
	}
	collectBoundLeaves(&bundle.Root, set.exact)
	for _, oi := range bundle.OperatorInputs {
		if oi.Path != "" {
			set.exact[oi.Path] = struct{}{}
		}
	}
	return set
}

// collectBoundLeaves recurses a layout node, adding every bound leaf
// path to dst. It reads the leaf-bearing fields the Lumencast runtime
// subscribes (tree.tsx resolveProps / useBindAnimate / KeyframePlayer):
// `bindings`, `animateBindings` values and `keyframes.key`.
func collectBoundLeaves(n *compiler.LayoutNode, dst map[string]struct{}) {
	for _, path := range n.Bindings {
		if path != "" {
			dst[path] = struct{}{}
		}
	}
	for _, path := range n.AnimateBindings {
		if path != "" {
			dst[path] = struct{}{}
		}
	}
	// keyframes.key (§6.6) — the KeyframePlayer replays on changes to
	// this leaf, so it is renderable. Decode just the `key` field.
	if len(n.Keyframes) > 0 {
		var kf struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal(n.Keyframes, &kf); err == nil && kf.Key != "" {
			dst[kf.Key] = struct{}{}
		}
	}
	for i := range n.Children {
		collectBoundLeaves(&n.Children[i], dst)
	}
}
