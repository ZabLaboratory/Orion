package compiler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// HTTPFetcher hits Canvas / Blue / components endpoints over HTTP.
// Production wires this against ZabGate (so the trust model stays
// gateway-first); tests construct a fake against an httptest server.
//
// The outbound service token is read **live at every request** via
// TokenFunc, not frozen at construction (ADR 002-B2 / Bastion C1). The
// service token manager rotates the access token on a background loop;
// capturing it once at boot meant the fetcher kept presenting the boot
// value (the static placeholder) forever, so every Canvas/Blue fetch
// 401'd and every push 422'd. getJSON calls TokenFunc() at call-time,
// mirroring the credentials proxy (internal/api/credentials.go:58).
type HTTPFetcher struct {
	Client     *http.Client
	CanvasBase string // e.g. http://zabgate:4000/canvas
	BlueBase   string // e.g. http://zabgate:4000/blue
	// TokenFunc returns Orion's current outbound service token, read
	// fresh on each request so a rotation is picked up immediately. Nil
	// is treated as "no token configured" (dev / tests that hit an
	// unauthenticated stub) — see getJSON.
	TokenFunc func() string
	UserAgent string
	// InjectAllowedHosts is a PREVIEW-ONLY escape hatch (embedded-local): when
	// a fetched layout has no ``assets.allowedHosts`` block, synthesise one
	// from the http(s) image hosts the layout itself references, so the
	// anti-SSRF authoring gate (GATE_HOST_NOT_ALLOWED, T1) lets the operator
	// preview a scene whose assets were authored against external hosts (e.g.
	// raw Figma asset URLs). Scoped to the scene's OWN hosts — not a blanket
	// allow — and NEVER set on the antenne profile. The bytes still fail to
	// load if the upstream host 404s; this only unblocks the compile.
	InjectAllowedHosts bool
}

// previewHostRe extracts http(s) hosts from a layout JSON blob for the
// embedded-local allowedHosts synthesis (InjectAllowedHosts).
var previewHostRe = regexp.MustCompile(`https?://([^/"'\s]+)`)

// previewBaselineHosts are CDNs scenes resolve at RUNTIME from data leaves
// (champion art), so they aren't in the static layout the host scan reads.
// Allowed in the embedded-local preview only (InjectAllowedHosts).
var previewBaselineHosts = []string{
	"ddragon.leagueoflegends.com",
	"ddragon.canisback.com",
}

// NewHTTPFetcher constructs a fetcher with sensible defaults. The
// serviceToken argument is a STATIC token captured into a closure for
// backward-compat with dev/test call sites that pass a fixed value (or
// "" for an unauthenticated stub). Production must use
// NewHTTPFetcherWithTokenFunc to wire the live, rotating token (C1).
func NewHTTPFetcher(canvasBase, blueBase, serviceToken string) *HTTPFetcher {
	var tf func() string
	if serviceToken != "" {
		tf = func() string { return serviceToken }
	}
	return NewHTTPFetcherWithTokenFunc(canvasBase, blueBase, tf)
}

// NewHTTPFetcherWithTokenFunc constructs a fetcher that reads its
// service token live from tokenFunc on every request (Bastion C1). In
// production cmd/orion wires tokenFunc to the ServiceTokenManager's
// Token method so a rotation is reflected on the next fetch.
func NewHTTPFetcherWithTokenFunc(canvasBase, blueBase string, tokenFunc func() string) *HTTPFetcher {
	return &HTTPFetcher{
		Client:     &http.Client{Timeout: 10 * time.Second},
		CanvasBase: strings.TrimRight(canvasBase, "/"),
		BlueBase:   strings.TrimRight(blueBase, "/"),
		TokenFunc:  tokenFunc,
		UserAgent:  "orion-compiler/1.0",
	}
}

// FetchCanvasLayout calls GET {canvas}/api/v1/layouts/{version}, then
// back-fills the literal ``defaults`` map from the content-addressed LSML
// bundle store.
//
// The ``/layouts`` adapter serves a CanvasLayout that carries the binding tree
// (``root``) but DROPS the bundle's ``defaults`` map — the constants every
// static text/image binds to (``__lit.<kind>.<id>`` → "BROKEN BLADE" /
// "assets/<sha>"). Without them a transcribed scene compiles to leaves nothing
// produces and paints empty (the gray-canvas symptom). The FULL bundle —
// including ``defaults`` — lives at ``GET /api/v1/lsml-bundles/{version}``, so
// we read its ``defaults`` and attach them to the layout. Best-effort: if the
// bundle store has no entry (e.g. a layout pushed without a stored bundle) the
// layout proceeds without literal defaults rather than failing the compile.
func (f *HTTPFetcher) FetchCanvasLayout(ctx context.Context, version string) (*CanvasLayout, error) {
	var out CanvasLayout
	url := f.CanvasBase + "/api/v1/layouts/" + version
	if err := f.getJSON(ctx, url, &out); err != nil {
		return nil, fmt.Errorf("canvas layout %s: %w", version, err)
	}
	if len(out.Defaults) == 0 {
		// The store wraps the bundle: `{content_hash, bundle:{lsml, layout,
		// defaults:{…}, …}, archive, …}`. The literal map lives at
		// `.bundle.defaults`, NOT top-level.
		var resp struct {
			Bundle struct {
				Defaults map[string]json.RawMessage `json:"defaults"`
			} `json:"bundle"`
		}
		burl := f.CanvasBase + "/api/v1/lsml-bundles/" + version
		if err := f.getJSON(ctx, burl, &resp); err == nil && len(resp.Bundle.Defaults) > 0 {
			out.Defaults = resp.Bundle.Defaults
		}
		// A missing / errored bundle store is non-fatal: the layout still
		// compiles (just without literal seeds), preserving the prior behaviour
		// for layouts that never had a stored bundle.
	}
	// PREVIEW-ONLY (embedded-local): synthesise allowedHosts from the scene's
	// own image hosts when the layout carries no assets block, so the SSRF
	// authoring gate doesn't reject a preview of an externally-hosted scene.
	if f.InjectAllowedHosts && len(out.Assets) == 0 {
		blob, _ := json.Marshal(struct {
			Root     LayoutNode                 `json:"root"`
			Defaults map[string]json.RawMessage `json:"defaults"`
		}{out.Root, out.Defaults})
		seen := map[string]struct{}{}
		// Baseline preview hosts: the champion/asset CDNs the scenes resolve at
		// RUNTIME from data leaves (e.g. `pl.*.champ` → a Data Dragon URL), so
		// they never appear in the static layout the scan below sees. Allowing
		// them lets Solar's runtime host-allow fetch champion art in preview.
		var hosts []string
		for _, h := range previewBaselineHosts {
			seen[h] = struct{}{}
			hosts = append(hosts, h)
		}
		for _, m := range previewHostRe.FindAllStringSubmatch(string(blob), -1) {
			h := strings.ToLower(m[1])
			if _, ok := seen[h]; ok {
				continue
			}
			seen[h] = struct{}{}
			hosts = append(hosts, h)
		}
		if len(hosts) > 0 {
			out.Assets, _ = json.Marshal(map[string][]string{"allowedHosts": hosts})
		}
	}
	return &out, nil
}

// FetchBlueprint resolves a blueprint's compute graph from Blue.
//
// Blue does NOT serve the graph on the blueprint row: GET
// /api/v1/blueprints/{id} returns BlueprintRead ({id, slug, status,
// current_version, interface, …}) with NO nodes/edges — the graph lives
// in the immutable version (blueprint_versions.graph). So this is a
// TWO-CALL fetch: read the blueprint to learn current_version, then read
// that version and lift its nested `graph.{nodes,edges}` into Orion's
// flat BlueprintGraph. (Same drift class as the compute-manifest envelope,
// issue #31 — unexercised because every live push so far was blueprint-
// free. BlueprintNode/Edge json tags already match Blue's graph schema:
// `definition`, `from_node`/`to_node`/`from_port`/`to_port`.)
func (f *HTTPFetcher) FetchBlueprint(ctx context.Context, id string) (*BlueprintGraph, error) {
	var meta struct {
		ID             string `json:"id"`
		CurrentVersion int    `json:"current_version"`
	}
	if err := f.getJSON(ctx, f.BlueBase+"/api/v1/blueprints/"+id, &meta); err != nil {
		return nil, fmt.Errorf("blue blueprint %s: %w", id, err)
	}
	var ver struct {
		Graph struct {
			Nodes []BlueprintNode `json:"nodes"`
			Edges []BlueprintEdge `json:"edges"`
			// Variables carry the blueprint-local CONSTANT declarations (e.g.
			// score-to-color's `palette` colour list). Previously dropped here —
			// only nodes+edges were decoded — so a reference-free top-level
			// blueprint's `variables[].value` never reached foldDeclaredVariables
			// → `core.variable.get@1` read an unseeded `__vars..<name>` → null
			// (the rating-colour squares rendered transparent). Decoding +
			// carrying them lets the compile seed `__vars..<name>` as a graph
			// default, same as the in-body simulate path already does.
			Variables []BlueprintVariable `json:"variables"`
		} `json:"graph"`
	}
	url := fmt.Sprintf("%s/api/v1/blueprints/%s/versions/%d", f.BlueBase, id, meta.CurrentVersion)
	if err := f.getJSON(ctx, url, &ver); err != nil {
		return nil, fmt.Errorf("blue blueprint %s version %d: %w", id, meta.CurrentVersion, err)
	}
	return &BlueprintGraph{
		ID:        meta.ID,
		Nodes:     ver.Graph.Nodes,
		Edges:     ver.Graph.Edges,
		Variables: ver.Graph.Variables,
	}, nil
}

// blueRefUnresolvedCodes are the typed Blue error codes that mean the
// pinned (blueprint_id, version) cannot be resolved to a published graph
// (Blue/docs/contracts/graph-resolution.md). Any of them maps to
// BLUEPRINT_REF_UNRESOLVED at the compiler (a push-time reject), never a
// retry or a current_version fall-back.
var blueRefUnresolvedCodes = map[string]struct{}{
	"BLUEPRINT_NOT_FOUND":             {},
	"BLUEPRINT_VERSION_NOT_FOUND":     {},
	"BLUEPRINT_VERSION_NOT_PUBLISHED": {},
}

// ErrRefUnresolved wraps a Blue typed error (404/422) that means a
// referenced (blueprint_id, version) is absent or unpublished. The compiler
// surfaces it as a BLUEPRINT_REF_UNRESOLVED diagnostic (ADR 014 §3.4). It is
// a distinct sentinel (distinct from the DiagnosticCode constant
// ErrBlueprintRefUnresolved) so the expansion pass can tell a hard "no such
// published graph" apart from a transport hiccup (which is FETCH_UPSTREAM).
var ErrRefUnresolved = errors.New("compiler: blueprint reference unresolved")

// FetchBlueprintGraph calls GET {blue}/api/v1/blueprints/{id}/versions/{version}/graph,
// the PINNED, published-only endpoint (Blue #94). Unlike FetchBlueprint it
// never reads current_version — the version is exactly the calling node's
// reference.version. A typed 404/422 (BLUEPRINT_NOT_FOUND /
// BLUEPRINT_VERSION_NOT_FOUND / BLUEPRINT_VERSION_NOT_PUBLISHED) is wrapped
// into ErrBlueprintRefUnresolved so the compiler fails the push closed with
// BLUEPRINT_REF_UNRESOLVED rather than silently substituting another version.
func (f *HTTPFetcher) FetchBlueprintGraph(ctx context.Context, id string, version int) (*ResolvedBlueprintGraph, error) {
	var out ResolvedBlueprintGraph
	url := fmt.Sprintf("%s/api/v1/blueprints/%s/versions/%d/graph", f.BlueBase, id, version)
	if err := f.getJSON(ctx, url, &out); err != nil {
		// A typed Blue error (404/422) means the pinned pair is absent or
		// unpublished — a hard reference-resolution failure. getStatusErr
		// carries the response body so we can read the top-level `code`.
		var se *statusError
		if errors.As(err, &se) {
			var typed struct {
				Code string `json:"code"`
			}
			if jerr := json.Unmarshal(se.body, &typed); jerr == nil {
				if _, unresolved := blueRefUnresolvedCodes[typed.Code]; unresolved {
					return nil, fmt.Errorf("blue blueprint %s version %d: %s: %w",
						id, version, typed.Code, ErrRefUnresolved)
				}
			}
		}
		return nil, fmt.Errorf("blue blueprint %s version %d graph: %w", id, version, err)
	}
	return &out, nil
}

// FetchComponent calls GET {canvas}/api/v1/components/{id}/{version}.
func (f *HTTPFetcher) FetchComponent(ctx context.Context, ref ComponentRef) (*UserComponent, error) {
	var out UserComponent
	url := fmt.Sprintf("%s/api/v1/components/%s/%s", f.CanvasBase, ref.ID, ref.Version)
	if err := f.getJSON(ctx, url, &out); err != nil {
		return nil, fmt.Errorf("canvas component %s@%s: %w", ref.ID, ref.Version, err)
	}
	return &out, nil
}

// FetchComputeManifest calls GET {blue}/api/v1/_compute-manifest and
// decodes Blue's real `{entries:[...],count}` envelope into Orion's
// compute-id → entry map.
//
// Blue does NOT serve a top-level `{computeId: entry}` map; it wraps
// the rows in an envelope and uses Blue-native field types (version is
// an int, declared_inputs is a list of dicts, declared_output_type may
// be a list). Decoding the response straight into ComputeManifest is
// what made every push fail COMPILE_FAILED — see issue #30 (found by
// the live E2E push on 2026-06-05). We decode into the Blue-shaped DTO
// first, then build the map keyed by node_id (namespace.name@version),
// which is the same ref a blueprint node carries in `definition`.
func (f *HTTPFetcher) FetchComputeManifest(ctx context.Context) (ComputeManifest, error) {
	var resp blueManifestResponse
	url := f.BlueBase + "/api/v1/_compute-manifest"
	if err := f.getJSON(ctx, url, &resp); err != nil {
		return nil, fmt.Errorf("blue compute manifest: %w", err)
	}
	return buildComputeManifest(resp), nil
}

// FetchEgressRoutes returns Blue's curated service-egress registry (ADR
// Blue 002 §3.2), read off the SAME `_compute-manifest` envelope. It is a
// separate call so the Fetcher interface stays unchanged (the compiler
// type-asserts this optional method); a fetcher that doesn't implement it
// yields a nil registry, which is fail-closed — every `core.service.call@1`
// node then rejects with EGRESS_ROUTE_NOT_DECLARED.
func (f *HTTPFetcher) FetchEgressRoutes(ctx context.Context) (EgressRegistry, error) {
	var resp blueManifestResponse
	url := f.BlueBase + "/api/v1/_compute-manifest"
	if err := f.getJSON(ctx, url, &resp); err != nil {
		return nil, fmt.Errorf("blue egress routes: %w", err)
	}
	return buildEgressRegistry(resp), nil
}

// buildEgressRegistry adapts Blue's wire egress DTOs into Orion's keyed
// registry, keyed by (service, route_id) — the same pair an authored node
// carries in config.
func buildEgressRegistry(resp blueManifestResponse) EgressRegistry {
	out := make(EgressRegistry, len(resp.EgressRoutes))
	for _, r := range resp.EgressRoutes {
		out[EgressRouteKey(r.Service, r.RouteID)] = EgressRoute(r)
	}
	return out
}

// buildComputeManifest adapts Blue's wire DTO into Orion's map. Keyed
// by node_id so validateBlueprint's `manifest[n.Compute]` lookup hits
// when the blueprint node's compute ref is `namespace.name@version`.
func buildComputeManifest(resp blueManifestResponse) ComputeManifest {
	out := make(ComputeManifest, len(resp.Entries))
	for _, e := range resp.Entries {
		out[e.NodeID] = ComputeManifestEntry{
			IsPure:             e.IsPure,
			IsBounded:          e.IsBounded,
			DeclaredInputs:     declaredInputNames(e.DeclaredInputs),
			DeclaredOutputType: flattenOutputType(e.DeclaredOutputType),
			Version:            strconv.Itoa(e.Version),
		}
	}
	return out
}

// declaredInputNames projects Blue's list-of-dict declared_inputs down
// to the input names Orion cares about. The compiler only enforces
// purity/boundedness off the manifest; the full port specs live in
// Blue. Keeping []string avoids churn on the downstream Go type while
// preserving the input identity.
func declaredInputNames(inputs []map[string]any) []string {
	if len(inputs) == 0 {
		return nil
	}
	names := make([]string, 0, len(inputs))
	for _, in := range inputs {
		if name, ok := in["name"].(string); ok && name != "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	return names
}

// flattenOutputType collapses Blue's `str | list[str] | null` output
// type into Orion's single string. A list (multiple outputs) is joined
// with ", " — the compiler treats this field as informational only, so
// a stable readable form is enough.
func flattenOutputType(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return strings.Join(list, ", ")
	}
	return ""
}

// ErrNoServiceToken is returned when the fetcher is in live mode (a
// TokenFunc is wired) but the current token is empty — e.g. the service
// token manager has not minted yet or a rotation produced an empty
// value. Per Bastion C2 the fetch fails explicitly rather than firing
// an opaque anonymous request that would 401 downstream with no signal.
var ErrNoServiceToken = errors.New("compiler: no service token available for outbound fetch")

func (f *HTTPFetcher) getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	// Bastion C1: read the token live, per request — never a value
	// captured at construction. A nil TokenFunc means "no auth wired"
	// (dev / unauthenticated stub) and sends an anonymous request, which
	// is the historical behaviour for the "" service-token case. A
	// non-nil TokenFunc means live mode: an empty result is a hard,
	// explicit failure (C2 — no silent anonymous fall-back). The token
	// value itself is never logged or wrapped into an error (C4).
	if f.TokenFunc != nil {
		token := f.TokenFunc()
		if token == "" {
			return ErrNoServiceToken
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("User-Agent", f.UserAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := f.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<14))
		return &statusError{status: resp.StatusCode, body: body}
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// statusError is a non-200 HTTP response. It carries the status code and a
// bounded body copy so callers (FetchBlueprintGraph) can inspect a typed
// error envelope — the body is never logged by getJSON itself (a token is
// never in a GET body, but the body may carry a Blue diagnostic the caller
// chooses to surface). Error() keeps the historical "status N: body" text so
// existing FETCH_UPSTREAM messages are unchanged.
type statusError struct {
	status int
	body   []byte
}

func (e *statusError) Error() string {
	return fmt.Sprintf("status %d: %s", e.status, e.body)
}
