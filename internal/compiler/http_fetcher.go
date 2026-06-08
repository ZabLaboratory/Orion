package compiler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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

// FetchCanvasLayout calls GET {canvas}/api/v1/layouts/{version}.
func (f *HTTPFetcher) FetchCanvasLayout(ctx context.Context, version string) (*CanvasLayout, error) {
	var out CanvasLayout
	url := f.CanvasBase + "/api/v1/layouts/" + version
	if err := f.getJSON(ctx, url, &out); err != nil {
		return nil, fmt.Errorf("canvas layout %s: %w", version, err)
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
		} `json:"graph"`
	}
	url := fmt.Sprintf("%s/api/v1/blueprints/%s/versions/%d", f.BlueBase, id, meta.CurrentVersion)
	if err := f.getJSON(ctx, url, &ver); err != nil {
		return nil, fmt.Errorf("blue blueprint %s version %d: %w", id, meta.CurrentVersion, err)
	}
	return &BlueprintGraph{ID: meta.ID, Nodes: ver.Graph.Nodes, Edges: ver.Graph.Edges}, nil
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
		return fmt.Errorf("status %d: %s", resp.StatusCode, body)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
