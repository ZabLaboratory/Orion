package compiler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

// FetchComputeManifest calls GET {blue}/api/v1/_compute-manifest
// per chantier-blue-extensions's contract.
func (f *HTTPFetcher) FetchComputeManifest(ctx context.Context) (ComputeManifest, error) {
	var out ComputeManifest
	url := f.BlueBase + "/api/v1/_compute-manifest"
	if err := f.getJSON(ctx, url, &out); err != nil {
		return nil, fmt.Errorf("blue compute manifest: %w", err)
	}
	return out, nil
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
