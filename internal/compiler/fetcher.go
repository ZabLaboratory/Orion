package compiler

import "context"

// Fetcher is what the compiler calls to pull authored artefacts at
// push time. Production wires HTTP clients against Canvas/Blue;
// tests substitute in-memory fakes.
//
// The compiler is purely synchronous: every fetch happens within the
// push request's deadline (ADR 004 targets ≤ 200 ms total). If a
// fetch fails the push fails — there is no retry / cache fall-back
// at compile time, the operator just retries.
type Fetcher interface {
	// FetchCanvasLayout retrieves the layout artefact for a published
	// canvas version (Canvas's own push notion).
	FetchCanvasLayout(ctx context.Context, canvasVersion string) (*CanvasLayout, error)

	// FetchBlueprint retrieves the Blue blueprint by its current
	// pushed-version id (the operator picks this in the editor).
	FetchBlueprint(ctx context.Context, blueprintID string) (*BlueprintGraph, error)

	// FetchBlueprintGraph retrieves a PINNED (blueprint_id, version)
	// published graph for compile-time reference expansion (ADR 014).
	// Unlike FetchBlueprint it never resolves current_version: the version
	// is exactly the `reference.version` of the calling node. An absent /
	// unpublished version is a hard, typed failure (BLUEPRINT_REF_UNRESOLVED
	// surfaced by the caller) — never a silent substitution
	// (Blue/docs/contracts/graph-resolution.md). It returns the raw nodes/
	// edges PLUS the version's declared interface (the input/output pin
	// names the call node's edges map onto) and the served purity (consumed,
	// never recomputed — ADR 006).
	FetchBlueprintGraph(ctx context.Context, blueprintID string, version int) (*ResolvedBlueprintGraph, error)

	// FetchComponent retrieves one pushed user-component version.
	// The compiler resolves these recursively, walking
	// component-uses-component edges, with cycle detection.
	FetchComponent(ctx context.Context, ref ComponentRef) (*UserComponent, error)

	// FetchComputeManifest returns Blue's per-compute manifest. Used
	// to enforce purity (criterion 18) — the compiler rejects any
	// node whose compute is not flagged is_pure: true.
	FetchComputeManifest(ctx context.Context) (ComputeManifest, error)
}
