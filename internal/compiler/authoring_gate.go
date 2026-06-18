package compiler

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/Lumencast/lumencast-go/lsml"
)

// Authoring validation gate (ADR 002 §3.4 T6 / #I ; Bastion conditions
// 1.2 T5+T6, threat-model 2026-06-17).
//
// The gate refuses an LSML bundle BEFORE it reaches the antenna when it
// violates a 0-loss / security invariant. It runs at the authoring→Orion
// frontier (scenes_push.go), on the EMITTED LSML bundle — the artefact
// that scenes_get.go serves to Solar. A bundle that trips any `error`
// here is NOT persisted and NEVER served: the author gets a clear refusal
// (422 LSML_GATE_REJECTED), never a silent drop or partial render.
//
// Defence-in-depth, not the only barrier: Solar's runtime re-gates every
// host/scheme/enum at render (host-allow.ts, css-color.ts) because live
// LSDP deltas reach the runtime without passing through the compiler. The
// figma exporter ALSO gates at export (validate.ts) for early author
// feedback. The gate here is the most AUTHORITATIVE "before the antenna"
// point — Orion re-validates INDEPENDENTLY and trusts no upstream
// diagnostic that travelled on the wire (arbitrage: Orion revalidates =
// defence in depth, see ADR 002 §3.4 / #I notes).
//
// What it refuses:
//   - T1/T2 — an image `src` / `mask.source` whose host is not in
//     bundle.assets.allowedHosts, or whose scheme is not https / bounded
//     data:image/* (GATE_HOST_NOT_ALLOWED / GATE_SCHEME_NOT_ALLOWED).
//   - T4 — a `blendMode` / `mask.type` / `mask.op` / `objectFit` outside
//     the closed enum (GATE_ENUM_NOT_ALLOWED).
//   - #K — a `mask.source.kind=="shape"` whose `ref` names no node id in
//     the bundle (GATE_DANGLING_MASK_REF), or a mask→shape→mask cycle
//     (GATE_MASK_CYCLE).
//   - T5 — the complexity budget: too many blend-bearing nodes, mask
//     nesting too deep, too many image nodes, or too many total nodes
//     (GATE_BUDGET_EXCEEDED). The blur cap is NOT re-opened here — it is
//     owned by the existing MAX_FILTER_BLUR_PX=100 clamp upstream.
//
// #N (SVG-unsanitizable → error) is enforced at the figma export gate
// (svg-sanitize.ts §7): an SVG that cannot be rebuilt never produces a
// `data:image/svg+xml` src, so it cannot reach this bundle. The gate here
// additionally refuses any `data:image/svg+xml` src outright (it must
// never appear post-sanitize), closing the path defensively.

// Authoring-gate diagnostic codes (T5/T6, #I). Stable; operators triage
// on them.
const (
	GateHostNotAllowed   DiagnosticCode = "GATE_HOST_NOT_ALLOWED"
	GateSchemeNotAllowed DiagnosticCode = "GATE_SCHEME_NOT_ALLOWED"
	GateEnumNotAllowed   DiagnosticCode = "GATE_ENUM_NOT_ALLOWED"
	GateDanglingMaskRef  DiagnosticCode = "GATE_DANGLING_MASK_REF"
	GateMaskCycle        DiagnosticCode = "GATE_MASK_CYCLE"
	GateBudgetExceeded   DiagnosticCode = "GATE_BUDGET_EXCEEDED"
	GateMalformedNode    DiagnosticCode = "GATE_MALFORMED_NODE"
)

// AuthoringBudget caps the complexity of one bundle (T5 anti-DoS). The
// numbers are measured in CI against the conformance fixture 817:3 and the
// pathological fixtures (#I): a real broadcast scene (817:3, ~190 image
// tiles + a handful of blend/mask nodes) sits comfortably under every cap,
// while the pathological fixtures trip exactly one each. They are headroom,
// not a tight fit — a legitimate authoring tree must never approach them.
//
// Rationale for the chosen numbers:
//   - MaxBlendNodes 512: 817:3 uses ~20 blend-bearing nodes; 512 is 25× the
//     real ceiling. mix-blend-mode forces a stacking context + offscreen
//     compositing per node — the runtime cost driver (R1). Beyond a few
//     hundred the CEF compositor thrashes.
//   - MaxMaskDepth 16: a real mask is 1–2 deep (a group masked by a sibling
//     shape). 16 bounds the recursive mask-resolution walk well below any
//     stack risk; a mask→shape→mask chain deeper than this is pathological.
//   - MaxImageNodes 4096: 817:3's pavage is ~190 <img>. 4096 is 20× that —
//     covers any legitimate tiled cover while bounding the DOM image count.
//   - MaxTotalNodes 16384: the whole 817:3 tree is ~600 nodes. 16384 bounds
//     the tree walk + the served DOM node count linearly.
type AuthoringBudget struct {
	MaxBlendNodes int
	MaxMaskDepth  int
	MaxImageNodes int
	MaxTotalNodes int
}

// DefaultAuthoringBudget is the production budget. Exported so tests and a
// future ops override can reference the canonical caps.
var DefaultAuthoringBudget = AuthoringBudget{
	MaxBlendNodes: 512,
	MaxMaskDepth:  16,
	MaxImageNodes: 4096,
	MaxTotalNodes: 16384,
}

// Closed enums (T4). Mirror @lumencast/compiler lsml-1_2.ts verbatim — the
// single source of truth is the spec; both SDKs must agree (R3 cross-SDK
// drift). A value outside these is an authoring error, never passthrough.
var (
	blendModes = newStringSet(
		"normal", "multiply", "screen", "overlay", "darken", "lighten",
		"color-dodge", "color-burn", "hard-light", "soft-light",
		"difference", "exclusion", "hue", "saturation", "color", "luminosity",
	)
	objectFits = newStringSet("cover", "contain", "fill", "none", "scale-down")
	maskTypes  = newStringSet("alpha", "luminance")
	maskOps    = newStringSet("intersect", "subtract", "union")
)

// allowedDataImageRe mirrors host-allow.ts ALLOWED_DATA_IMAGE_RE (T2): the
// only `data:` payloads admitted are bounded raster images — never
// text/html, wildcard, or image/svg+xml (an SVG data: URI must have been
// sanitized to a rebuilt document by #N; raw SVG bytes never reach here).
var allowedDataImageRe = regexp.MustCompile(`(?i)^data:image/(png|jpeg|jpg|gif|webp|avif|bmp|x-icon);base64,`)

const maxURLLen = 8192 // mirrors host-allow.ts MAX_URL_LEN (anti-DoS).

// GateLSMLBundle runs the authoring validation gate over an emitted LSML
// bundle with the production budget. It returns a Diagnostics bag; the
// caller refuses the push when HasErrors() is true. It is a PURE function:
// it never mutates the bundle, never logs, never echoes a rejected URL
// (Bastion R9 — reasons are static, paths carry only the node location).
func GateLSMLBundle(b *lsml.Bundle) Diagnostics {
	return gateLSMLBundle(b, DefaultAuthoringBudget)
}

func gateLSMLBundle(b *lsml.Bundle, budget AuthoringBudget) Diagnostics {
	var d Diagnostics
	if b == nil {
		d.AddError(GateMalformedNode, "nil bundle")
		return d
	}

	var allowed []string
	if b.Assets != nil {
		allowed = b.Assets.AllowedHosts
	}

	// First pass: parse the opaque layout tree and collect every node id so
	// shape-ref masks can be checked against real ids (#K dangling ref).
	var root gateNode
	if len(b.Layout) > 0 {
		if err := json.Unmarshal(b.Layout, &root); err != nil {
			d.AddError(GateMalformedNode, "layout is not a valid node tree: %v", err)
			return d
		}
	}
	ids := map[string]bool{}
	nodesByID := map[string]*gateNode{}
	collectIDs(&root, ids, nodesByID)

	w := &gateWalk{
		diags:     &d,
		allowed:   allowed,
		ids:       ids,
		nodesByID: nodesByID,
		budget:    budget,
	}
	w.walk(&root, "/layout")

	if w.totalNodes > budget.MaxTotalNodes {
		d.AddErrorAt(GateBudgetExceeded, "/layout",
			"node count %d exceeds budget %d (T5)", w.totalNodes, budget.MaxTotalNodes)
	}
	if w.blendNodes > budget.MaxBlendNodes {
		d.AddErrorAt(GateBudgetExceeded, "/layout",
			"blend-bearing node count %d exceeds budget %d (T5)", w.blendNodes, budget.MaxBlendNodes)
	}
	if w.imageNodes > budget.MaxImageNodes {
		d.AddErrorAt(GateBudgetExceeded, "/layout",
			"image node count %d exceeds budget %d (T5)", w.imageNodes, budget.MaxImageNodes)
	}
	return d
}

// gateNode is a partial view of an LSML node: only the fields the gate
// inspects. Unknown props are ignored (additive 1.2 forward-compat).
type gateNode struct {
	Kind        string     `json:"kind"`
	ID          string     `json:"id"`
	BlendMode   *string    `json:"blendMode"`
	ObjectFit   *string    `json:"objectFit"`
	Src         *string    `json:"src"`
	Mask        *gateMask  `json:"mask"`
	Fills       []gateFill `json:"fills"`
	Backgrounds []gateFill `json:"backgrounds"`
	Children    []gateNode `json:"children"`
}

type gateMask struct {
	Source *gateMaskSource `json:"source"`
	Type   *string         `json:"type"`
	Op     *string         `json:"op"`
}

type gateMaskSource struct {
	Kind string  `json:"kind"`
	Ref  *string `json:"ref"`
	Src  *string `json:"src"`
}

type gateFill struct {
	Kind      string  `json:"kind"`
	Src       *string `json:"src"`
	ObjectFit *string `json:"objectFit"`
	BlendMode *string `json:"blendMode"`
}

func collectIDs(n *gateNode, ids map[string]bool, byID map[string]*gateNode) {
	if n.ID != "" {
		ids[n.ID] = true
		byID[n.ID] = n
	}
	for i := range n.Children {
		collectIDs(&n.Children[i], ids, byID)
	}
}

type gateWalk struct {
	diags     *Diagnostics
	allowed   []string
	ids       map[string]bool
	nodesByID map[string]*gateNode
	budget    AuthoringBudget

	totalNodes int
	blendNodes int
	imageNodes int
}

// walk validates one node and recurses over the tree.
func (w *gateWalk) walk(n *gateNode, path string) {
	w.totalNodes++

	// blendMode (T4) — node level + per-fill below.
	bearsBlend := false
	if n.BlendMode != nil {
		bearsBlend = true
		if !blendModes.Has(*n.BlendMode) {
			w.diags.AddErrorAt(GateEnumNotAllowed, path+"/blendMode",
				"blendMode %q is not a recognised mode (T4)", *n.BlendMode)
		}
	}
	if n.ObjectFit != nil && !objectFits.Has(*n.ObjectFit) {
		w.diags.AddErrorAt(GateEnumNotAllowed, path+"/objectFit",
			"objectFit %q is not a recognised value (T4)", *n.ObjectFit)
	}

	// image node `src` (T1/T2).
	if n.Src != nil {
		w.checkSrc(*n.Src, path+"/src")
	}
	if n.Kind == "image" {
		w.imageNodes++
	}

	// fills[] / backgrounds[] (T1/T2 src, T4 enums).
	w.checkFills(n.Fills, path+"/fills", &bearsBlend)
	w.checkFills(n.Backgrounds, path+"/backgrounds", &bearsBlend)

	if bearsBlend {
		w.blendNodes++
	}

	// mask (T4 enums, T1/T2 image source, #K dangling ref + cycle, T5 depth).
	if n.Mask != nil {
		// The mask resolution chain starts at this node's own id (when it
		// has one) so a node masked by a shape that loops back to it is
		// caught (mask→shape→mask, #K).
		var chain []string
		if n.ID != "" {
			chain = []string{n.ID}
		}
		w.checkMask(n.Mask, path+"/mask", 0, chain)
	}

	for i := range n.Children {
		w.walk(&n.Children[i], fmt.Sprintf("%s/children/%d", path, i))
	}
}

func (w *gateWalk) checkFills(fills []gateFill, path string, bearsBlend *bool) {
	for i, f := range fills {
		fp := fmt.Sprintf("%s/%d", path, i)
		if f.BlendMode != nil {
			*bearsBlend = true
			if !blendModes.Has(*f.BlendMode) {
				w.diags.AddErrorAt(GateEnumNotAllowed, fp+"/blendMode",
					"fill blendMode %q is not a recognised mode (T4)", *f.BlendMode)
			}
		}
		if f.ObjectFit != nil && !objectFits.Has(*f.ObjectFit) {
			w.diags.AddErrorAt(GateEnumNotAllowed, fp+"/objectFit",
				"fill objectFit %q is not a recognised value (T4)", *f.ObjectFit)
		}
		if f.Kind == "image" && f.Src != nil {
			w.checkSrc(*f.Src, fp+"/src")
		}
	}
}

// checkMask validates one mask spec and, for shape sources, FOLLOWS the
// reference chain: a node masked by shape S, where S itself carries a mask
// whose source is another shape, and so on. `maskDepth` is the length of
// that chain (T5 cap); `maskChain` is the set of node ids already on it
// (#K cycle). The walk terminates because every step either hits a
// non-shape mask, a dangling ref, the depth cap, or a repeated id.
func (w *gateWalk) checkMask(m *gateMask, path string, maskDepth int, maskChain []string) {
	if m.Type != nil && !maskTypes.Has(*m.Type) {
		w.diags.AddErrorAt(GateEnumNotAllowed, path+"/type",
			"mask.type %q is not a recognised value (T4)", *m.Type)
	}
	if m.Op != nil && !maskOps.Has(*m.Op) {
		w.diags.AddErrorAt(GateEnumNotAllowed, path+"/op",
			"mask.op %q is not a recognised value (T4)", *m.Op)
	}
	if maskDepth+1 > w.budget.MaxMaskDepth {
		w.diags.AddErrorAt(GateBudgetExceeded, path,
			"mask nesting depth %d exceeds budget %d (T5)", maskDepth+1, w.budget.MaxMaskDepth)
		return
	}
	if m.Source == nil {
		return
	}
	switch m.Source.Kind {
	case "image":
		if m.Source.Src != nil {
			w.checkSrc(*m.Source.Src, path+"/source/src")
		}
	case "shape":
		if m.Source.Ref == nil || *m.Source.Ref == "" {
			w.diags.AddErrorAt(GateDanglingMaskRef, path+"/source/ref",
				"mask shape source has no ref (#K)")
			return
		}
		ref := *m.Source.Ref
		if !w.ids[ref] {
			w.diags.AddErrorAt(GateDanglingMaskRef, path+"/source/ref",
				"mask references unknown node id %q (#K)", ref)
			return
		}
		// Cycle: the referenced shape is already on the current mask
		// resolution path (mask→shape→mask…). Refuse rather than recurse
		// into a non-terminating walk (#K anti-cycle).
		for _, seen := range maskChain {
			if seen == ref {
				w.diags.AddErrorAt(GateMaskCycle, path+"/source/ref",
					"mask cycle through node id %q (#K)", ref)
				return
			}
		}
		// Follow the chain: if the referenced shape is itself masked by a
		// shape, that extends the resolution path. The referenced node's
		// own props were validated where it lives in the tree (the walk
		// visits it); here we only chase the mask-source dependency for
		// depth + cycle.
		target := w.nodesByID[ref]
		if target != nil && target.Mask != nil {
			w.checkMask(target.Mask, path+"/source→"+ref+"/mask",
				maskDepth+1, append(maskChain, ref))
		}
	default:
		w.diags.AddErrorAt(GateMalformedNode, path+"/source",
			"mask source kind %q is not shape|image", m.Source.Kind)
	}
}

// checkSrc enforces T2 (scheme) then T1 (host) on one untrusted asset URL.
// Mirrors host-allow.ts: bounded data:image/* skips host; any other data:
// (text/html/svg/wildcard) is rejected; https must host-match the bundle's
// allowedHosts on the PARSED hostname (never a substring). The rejected URL
// is never echoed into the diagnostic (Bastion R9).
func (w *gateWalk) checkSrc(raw, path string) {
	if len(raw) == 0 || len(raw) > maxURLLen {
		w.diags.AddErrorAt(GateSchemeNotAllowed, path,
			"asset url is empty or exceeds the length cap (T2)")
		return
	}
	if allowedDataImageRe.MatchString(raw) {
		return // bounded inline raster — no host, no network.
	}
	if strings.HasPrefix(strings.ToLower(raw), "data:") {
		w.diags.AddErrorAt(GateSchemeNotAllowed, path,
			"data: url is not a bounded image/* payload (T2)")
		return
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		w.diags.AddErrorAt(GateSchemeNotAllowed, path,
			"asset url is not a parseable absolute URL (T2)")
		return
	}
	if strings.ToLower(u.Scheme) != "https" {
		w.diags.AddErrorAt(GateSchemeNotAllowed, path,
			"asset url scheme is not https (T2)")
		return
	}
	// Userinfo (`trusted.com@evil.com`) splits host from the trusted name:
	// url.Hostname() already returns the real host, but reject userinfo
	// outright — it never belongs on an asset URL and is a known confusion.
	if u.User != nil {
		w.diags.AddErrorAt(GateHostNotAllowed, path,
			"asset url carries userinfo (T1)")
		return
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || !hostMatches(host, w.allowed) {
		w.diags.AddErrorAt(GateHostNotAllowed, path,
			"asset host is not in assets.allowedHosts (T1)")
		return
	}
}

// hostMatches does an exact, case-insensitive equality against each
// allowlist entry on the PARSED hostname — never a substring, so
// `trusted.com.evil.com` / `trusted-com.evil.com` are rejected. An
// empty allowlist denies every remote host (deny-by-default).
func hostMatches(host string, allowed []string) bool {
	for _, a := range allowed {
		if host == strings.ToLower(strings.TrimSpace(a)) {
			return true
		}
	}
	return false
}

type stringSet map[string]struct{}

func newStringSet(values ...string) stringSet {
	s := make(stringSet, len(values))
	for _, v := range values {
		s[v] = struct{}{}
	}
	return s
}

func (s stringSet) Has(v string) bool {
	_, ok := s[v]
	return ok
}
