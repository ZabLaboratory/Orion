package compiler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
)

// ErrBundleMiss is returned when the frozen bundle has no artefact at the
// requested key. It is a hard fetch failure (fail-closed): the compiler
// surfaces it as a FETCH_UPSTREAM-class push error, never a silent skip.
// A FetchComponent miss is the expected, correct outcome for the
// component-free canvas-chat-sponso scene (§B.4) — the compiler simply
// never asks for one.
var ErrBundleMiss = errors.New("compiler: artefact absent from frozen scene bundle")

// BundledFetcher is the embedded-local Fetcher (ADR 016 §3.2 / §B, issue
// #224). It serves the FROZEN artefacts of the embedded-local default scene
// (the canvas-chat-sponso layout + its materialised blueprints) from an
// on-disk bundle instead of HTTP calls to Canvas/Blue. The compiler is
// unchanged: BundledFetcher returns the SAME Go structs HTTPFetcher decodes
// from the live 200 bodies, so the compile path stays byte-identical between
// antenne and embedded-local (the single rule of the Conduit contract:
// substitute the transport, never the shape — docs/contracts/
// embedded-local-contracts.md §B).
//
// Substitution is fail-closed: a bundle miss is a hard fetch error, never a
// silent fall-back. In particular the two-call FetchBlueprint of the HTTP
// path is collapsed into one local id→graph lookup (§B.2), and the pinned
// FetchBlueprintGraph reproduces the typed BLUEPRINT_* errors so the
// compiler still rejects an unresolved reference with BLUEPRINT_REF_UNRESOLVED
// (§B.3) — a missing artefact fails closed, never substitutes another version.
type BundledFetcher struct {
	bundle SceneBundle
}

// SceneBundle is the on-disk frozen bundle, the static substitute for the
// Canvas/Blue HTTP surface in embedded-local. Each map is keyed exactly as
// the compiler addresses the artefact:
//
//   - CanvasLayouts: by the bare 64-hex content address (== CanvasVersion).
//   - Blueprints: by blueprint id — the value is the already-collapsed flat
//     graph (the §B.2 single-lookup form of the two-call HTTP fetch).
//   - BlueprintGraphs: by "<id>@<version>" — the pinned, published resolved
//     graphs for ADR 014 reference expansion (§B.3).
//   - Components: by "<id>@<version>" (the canvas-chat-sponso scene uses
//     none, so this is typically empty — a FetchComponent miss is correct).
//   - ComputeManifest: the verbatim Blue `{entries,count}` envelope (§B.4).
//
// Every artefact is captured verbatim from the live 200 body per the freeze
// recipe; BundledFetcher only re-keys and re-decodes — no transform.
type SceneBundle struct {
	CanvasLayouts   map[string]json.RawMessage `json:"canvas_layouts"`
	Blueprints      map[string]json.RawMessage `json:"blueprints"`
	BlueprintGraphs map[string]json.RawMessage `json:"blueprint_graphs"`
	Components      map[string]json.RawMessage `json:"components,omitempty"`
	ComputeManifest json.RawMessage            `json:"compute_manifest"`
}

// compile-time assertion: BundledFetcher satisfies the Fetcher interface.
var _ Fetcher = (*BundledFetcher)(nil)

// NewBundledFetcher builds a fetcher over an in-memory bundle.
func NewBundledFetcher(bundle SceneBundle) *BundledFetcher {
	return &BundledFetcher{bundle: bundle}
}

// LoadSceneBundle reads and decodes a frozen scene bundle from disk. The
// boot path (cmd/orion in embedded-local) calls this once; a malformed or
// missing bundle is a hard boot error (the profile cannot run without it).
func LoadSceneBundle(path string) (SceneBundle, error) {
	var b SceneBundle
	data, err := os.ReadFile(path)
	if err != nil {
		return b, fmt.Errorf("scene bundle %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &b); err != nil {
		return b, fmt.Errorf("scene bundle %s: decode: %w", path, err)
	}
	return b, nil
}

// FetchCanvasLayout returns the frozen layout for a canvas version (§B.1).
// The blob is decoded through the real CanvasLayout struct — animations and
// assets ride along as opaque bytes, exactly as the HTTP path leaves them.
func (f *BundledFetcher) FetchCanvasLayout(_ context.Context, version string) (*CanvasLayout, error) {
	raw, ok := f.bundle.CanvasLayouts[version]
	if !ok {
		return nil, fmt.Errorf("canvas layout %s: %w", version, ErrBundleMiss)
	}
	var out CanvasLayout
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("canvas layout %s: decode: %w", version, err)
	}
	return &out, nil
}

// FetchBlueprint returns the frozen blueprint graph by id (§B.2). The HTTP
// path is a two-call fetch (blueprint row → current_version graph); the
// frozen form collapses both into one local lookup, returning the same flat
// BlueprintGraph the compiler decodes.
func (f *BundledFetcher) FetchBlueprint(_ context.Context, id string) (*BlueprintGraph, error) {
	raw, ok := f.bundle.Blueprints[id]
	if !ok {
		return nil, fmt.Errorf("blue blueprint %s: %w", id, ErrBundleMiss)
	}
	var out BlueprintGraph
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("blue blueprint %s: decode: %w", id, err)
	}
	return &out, nil
}

// FetchBlueprintGraph returns the pinned, published resolved graph for a
// (blueprint_id, version) reference (§B.3). An absent pinned pair is a hard
// reference-resolution failure wrapped in ErrRefUnresolved — the SAME
// sentinel the HTTP path raises on a typed BLUEPRINT_* Blue error — so the
// compiler fails the push closed with BLUEPRINT_REF_UNRESOLVED rather than
// silently substituting another version (the frozen-bundle fail-closed
// invariant; Blue docs/contracts/graph-resolution.md).
func (f *BundledFetcher) FetchBlueprintGraph(_ context.Context, id string, version int) (*ResolvedBlueprintGraph, error) {
	key := id + "@" + strconv.Itoa(version)
	raw, ok := f.bundle.BlueprintGraphs[key]
	if !ok {
		return nil, fmt.Errorf("blue blueprint %s version %d: %s: %w",
			id, version, "BLUEPRINT_VERSION_NOT_FOUND", ErrRefUnresolved)
	}
	var out ResolvedBlueprintGraph
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("blue blueprint %s version %d graph: decode: %w", id, version, err)
	}
	return &out, nil
}

// FetchComponent returns a frozen user component by (id, version) (§B.4).
// The canvas-chat-sponso scene declares no components, so the bundle
// typically carries none — a miss is then a correct hard fetch error (the
// frozen scene uses no components, so the compiler should never ask).
func (f *BundledFetcher) FetchComponent(_ context.Context, ref ComponentRef) (*UserComponent, error) {
	key := ref.ID + "@" + ref.Version
	raw, ok := f.bundle.Components[key]
	if !ok {
		return nil, fmt.Errorf("canvas component %s@%s: %w", ref.ID, ref.Version, ErrBundleMiss)
	}
	var out UserComponent
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("canvas component %s@%s: decode: %w", ref.ID, ref.Version, err)
	}
	return &out, nil
}

// FetchComputeManifest returns the frozen Blue compute manifest (§B.4),
// decoded through the SAME wire DTO + adapter the HTTP path uses, so the
// keyed-by-node_id map the compiler enforces purity against is identical.
func (f *BundledFetcher) FetchComputeManifest(_ context.Context) (ComputeManifest, error) {
	if len(f.bundle.ComputeManifest) == 0 {
		return nil, fmt.Errorf("blue compute manifest: %w", ErrBundleMiss)
	}
	var resp blueManifestResponse
	if err := json.Unmarshal(f.bundle.ComputeManifest, &resp); err != nil {
		return nil, fmt.Errorf("blue compute manifest: decode: %w", err)
	}
	return buildComputeManifest(resp), nil
}

// FetchEgressRoutes returns the curated egress registry frozen into the
// bundle's compute manifest (ADR Blue 002 §3.2). An absent / empty bundle
// manifest yields a nil registry — fail-closed (every `core.service.call@1`
// then rejects EGRESS_ROUTE_NOT_DECLARED).
func (f *BundledFetcher) FetchEgressRoutes(_ context.Context) (EgressRegistry, error) {
	if len(f.bundle.ComputeManifest) == 0 {
		return nil, nil
	}
	var resp blueManifestResponse
	if err := json.Unmarshal(f.bundle.ComputeManifest, &resp); err != nil {
		return nil, fmt.Errorf("blue egress routes: decode: %w", err)
	}
	return buildEgressRegistry(resp), nil
}
