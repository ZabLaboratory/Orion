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

	// FetchComponent retrieves one pushed user-component version.
	// The compiler resolves these recursively, walking
	// component-uses-component edges, with cycle detection.
	FetchComponent(ctx context.Context, ref ComponentRef) (*UserComponent, error)

	// FetchComputeManifest returns Blue's per-compute manifest. Used
	// to enforce purity (criterion 18) — the compiler rejects any
	// node whose compute is not flagged is_pure: true.
	FetchComputeManifest(ctx context.Context) (ComputeManifest, error)
}
