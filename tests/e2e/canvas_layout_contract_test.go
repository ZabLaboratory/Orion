//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/compiler"
)

// TestCanvasLayoutContract_RealHTTPRoundTrip exercises the ZabCanvas -> Orion
// CanvasLayout contract (issue #27, ADR-002 §6 C-min acceptance #1) through
// Orion's REAL consumer code path — not a canned struct decode.
//
// testdata/canvas_layout_from_canvas.json is byte-identical to the fixture
// ZabCanvas generates from its REAL adapter (ZabCanvas
// tests/test_layouts.py::test_emit_go_contract_fixture, adapting
// services/layout_adapter.py::adapt_bundle_to_layout — the same function
// GET /api/v1/layouts/{version} calls in production; ZabCanvas#24/PR#25).
// It ships in ZabCanvas at tests/contract/canvas_layout_from_canvas.json,
// alongside a sibling Go decode test whose README explicitly asks for this
// wiring ("Probe drops it into Orion ... to wire it into Orion CI").
//
// This test goes one step further than that sibling: instead of
// json.Unmarshal-ing the fixture directly into compiler.CanvasLayout and
// calling the unexported expandLayout, it serves the fixture bytes over a
// real httptest.Server standing in for ZabCanvas, and drives them through
// the real compiler.HTTPFetcher (the exact client Orion's push handler
// uses in production) and the real compiler.Compile (fetch + decode +
// expand + lower + emit) — the actual network-and-compile pipeline a push
// exercises, not a shortcut into internal state.
//
// Postgres: NOT used. The CanvasLayout contract is HTTP + JSON decode + an
// in-memory compile; nothing here touches a database (internal/store was
// removed in #346 — Orion is stateless on this path). It lives under the
// `e2e` build tag / tests/e2e/ because that is what ci.yml's `e2e
// (Postgres)` job runs (go test -tags e2e ./tests/e2e/...); the job's
// Postgres bootstrap is simply unused by this particular test — it is
// there for the CI substrate (ADR 018), not a requirement this test
// imposes. See tests/e2e/README.md for the full accounting of what does
// and does not have e2e coverage today.
func TestCanvasLayoutContract_RealHTTPRoundTrip(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "canvas_layout_from_canvas.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var probe struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(fixture, &probe); err != nil {
		t.Fatalf("fixture is not valid JSON: %v", err)
	}
	if probe.Version == "" {
		t.Fatal("fixture carries no version")
	}

	// Stand-in ZabCanvas + Blue routes, close enough to the real surface to
	// drive HTTPFetcher/Compile for real: a version-gated layout route (the
	// producer 404s a non-matching version — routes/layouts.py::_HASH_PATTERN
	// — so gating here the same way keeps the double honest), an empty
	// compute manifest (this scene is blueprint-free and component-free —
	// the ZabCanvas-authored sibling test's own words: "a static scene"),
	// and a 404 lsml-bundle store (this layout carries no literal
	// `defaults`, matching production for a layout with no static
	// text/image literals — FetchCanvasLayout treats that as non-fatal).
	var canvasHits, manifestHits int
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/layouts/"+probe.Version, func(w http.ResponseWriter, r *http.Request) {
		canvasHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	})
	mux.HandleFunc("/api/v1/layouts/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"detail":"not found"}`, http.StatusNotFound)
	})
	mux.HandleFunc("/api/v1/lsml-bundles/"+probe.Version, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"detail":"not found"}`, http.StatusNotFound)
	})
	mux.HandleFunc("/api/v1/_compute-manifest", func(w http.ResponseWriter, r *http.Request) {
		manifestHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"entries":[],"count":0}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	fetcher := compiler.NewHTTPFetcher(srv.URL, srv.URL, "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	graph, bundle, sceneVersion, err := compiler.Compile(ctx, "e2e-canvaslayout-contract", compiler.PushEnvelope{
		CanvasVersion: probe.Version,
	}, fetcher)
	if err != nil {
		t.Fatalf("Compile rejected the real ZabCanvas-shaped layout: %v", err)
	}
	if canvasHits == 0 {
		t.Fatal("GET /api/v1/layouts/{version} was never called — the fetcher did not hit the wire")
	}
	if manifestHits == 0 {
		t.Fatal("GET /api/v1/_compute-manifest was never called")
	}
	if graph == nil || bundle == nil || sceneVersion == "" {
		t.Fatalf("Compile returned an incomplete artefact: graph=%v bundle=%v sceneVersion=%q", graph, bundle, sceneVersion)
	}

	// Structural + content assertions on the LOWERED render bundle — the
	// same tree the fixture describes (root/frame -> col/stack ->
	// title/text + bar/shape) — proving the binding tree not only decoded
	// but survived the real lowering pass through to the served artefact,
	// not just a raw json.Unmarshal.
	root := bundle.Root
	if root.Kind != "frame" || root.ID != "root" {
		t.Fatalf("root mismatch: kind=%q id=%q", root.Kind, root.ID)
	}
	if len(root.Children) != 1 || root.Children[0].ID != "col" {
		t.Fatalf("root children mismatch: %+v", root.Children)
	}
	col := root.Children[0]
	if len(col.Children) != 2 || col.Children[0].ID != "title" || col.Children[1].ID != "bar" {
		t.Fatalf("col children mismatch: %+v", col.Children)
	}
	if got := col.Children[0].Bindings["value"]; got != "scoreboard.home.name" {
		t.Fatalf("title binding lost or renamed through compile: %+v", col.Children[0].Bindings)
	}
}
