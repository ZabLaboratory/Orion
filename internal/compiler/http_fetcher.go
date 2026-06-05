package compiler

import (
	"context"
	"encoding/json"
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
type HTTPFetcher struct {
	Client       *http.Client
	CanvasBase   string // e.g. http://zabgate:4000/canvas
	BlueBase     string // e.g. http://zabgate:4000/blue
	ServiceToken string // Orion's outbound service token
	UserAgent    string
}

// NewHTTPFetcher constructs a fetcher with sensible defaults.
func NewHTTPFetcher(canvasBase, blueBase, serviceToken string) *HTTPFetcher {
	return &HTTPFetcher{
		Client:       &http.Client{Timeout: 10 * time.Second},
		CanvasBase:   strings.TrimRight(canvasBase, "/"),
		BlueBase:     strings.TrimRight(blueBase, "/"),
		ServiceToken: serviceToken,
		UserAgent:    "orion-compiler/1.0",
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

// FetchBlueprint calls GET {blue}/api/v1/blueprints/{id}.
func (f *HTTPFetcher) FetchBlueprint(ctx context.Context, id string) (*BlueprintGraph, error) {
	var out BlueprintGraph
	url := f.BlueBase + "/api/v1/blueprints/" + id
	if err := f.getJSON(ctx, url, &out); err != nil {
		return nil, fmt.Errorf("blue blueprint %s: %w", id, err)
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

func (f *HTTPFetcher) getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if f.ServiceToken != "" {
		req.Header.Set("Authorization", "Bearer "+f.ServiceToken)
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
