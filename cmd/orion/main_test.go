package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/config"
)

// tokenStub is a no-op outbound-token source for selectFetcher tests.
func tokenStub() string { return "" }

// TestSelectFetcher_AntenneUsesHTTP pins RC-A1 §3 parity: the antenne profile
// always selects the live httpFetcher against the Canvas/Blue bases — never the
// bundle — even if a scene bundle path is (wrongly) present in the env. This is
// the rigorous-unchanged guard for the production fetch path.
func TestSelectFetcher_AntenneUsesHTTP(t *testing.T) {
	cfg := config.Config{
		Profile:       config.ProfileAntenne,
		CanvasBaseURL: "http://zabgate:4000/canvas",
		BlueBaseURL:   "http://zabgate:4000/blue",
		// A stray bundle path must NOT divert antenne to the bundle.
		SceneBundlePath: "/should/be/ignored.json",
	}
	f, source, err := selectFetcher(cfg, tokenStub)
	if err != nil {
		t.Fatalf("selectFetcher: %v", err)
	}
	if source != fetcherSourceHTTP {
		t.Fatalf("source = %q, want %q", source, fetcherSourceHTTP)
	}
	hf, ok := f.(*compiler.HTTPFetcher)
	if !ok {
		t.Fatalf("antenne fetcher = %T, want *compiler.HTTPFetcher", f)
	}
	if hf.CanvasBase != cfg.CanvasBaseURL || hf.BlueBase != cfg.BlueBaseURL {
		t.Fatalf("bases = %q/%q, want %q/%q", hf.CanvasBase, hf.BlueBase, cfg.CanvasBaseURL, cfg.BlueBaseURL)
	}
}

// TestSelectFetcher_EmbeddedLocalNominalHTTP proves RC-A1 §1/§2: in
// embedded-local with NO bundle path, the httpFetcher is selected and pointed
// at the loopback gateway sidecar bases — the same fetcher the antenne path
// builds, only the bases differ. This is the scene-agnostic nominal path.
func TestSelectFetcher_EmbeddedLocalNominalHTTP(t *testing.T) {
	cfg := config.Config{
		Profile:       config.ProfileEmbeddedLocal,
		CanvasBaseURL: "http://127.0.0.1:4000/canvas",
		BlueBaseURL:   "http://127.0.0.1:4000/blue",
		// SceneBundlePath deliberately empty: nominal HTTP path.
	}
	f, source, err := selectFetcher(cfg, tokenStub)
	if err != nil {
		t.Fatalf("selectFetcher: %v", err)
	}
	if source != fetcherSourceHTTP {
		t.Fatalf("source = %q, want %q", source, fetcherSourceHTTP)
	}
	hf, ok := f.(*compiler.HTTPFetcher)
	if !ok {
		t.Fatalf("embedded-local nominal fetcher = %T, want *compiler.HTTPFetcher", f)
	}
	if hf.CanvasBase != cfg.CanvasBaseURL || hf.BlueBase != cfg.BlueBaseURL {
		t.Fatalf("loopback bases = %q/%q, want %q/%q", hf.CanvasBase, hf.BlueBase, cfg.CanvasBaseURL, cfg.BlueBaseURL)
	}
}

// TestSelectFetcher_EmbeddedLocalBundleFallback proves the offline fallback is
// retained (RC-A1 §2): in embedded-local WITH a bundle path, the bundledFetcher
// is selected and no gateway is contacted.
func TestSelectFetcher_EmbeddedLocalBundleFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bundle.json")
	// Minimal well-formed bundle (empty maps decode fine).
	body, err := json.Marshal(compiler.SceneBundle{})
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
	cfg := config.Config{
		Profile:         config.ProfileEmbeddedLocal,
		CanvasBaseURL:   "http://127.0.0.1:4000/canvas",
		BlueBaseURL:     "http://127.0.0.1:4000/blue",
		SceneBundlePath: path,
	}
	f, source, err := selectFetcher(cfg, tokenStub)
	if err != nil {
		t.Fatalf("selectFetcher: %v", err)
	}
	if source != fetcherSourceBundle {
		t.Fatalf("source = %q, want %q", source, fetcherSourceBundle)
	}
	if _, ok := f.(*compiler.BundledFetcher); !ok {
		t.Fatalf("embedded-local fallback fetcher = %T, want *compiler.BundledFetcher", f)
	}
}

// TestSelectFetcher_EmbeddedLocalMissingBundleIsHardError — a configured but
// unreadable bundle path is a hard boot error, never a silent fall-through to
// HTTP (fail-closed: the operator asked for the offline path).
func TestSelectFetcher_EmbeddedLocalMissingBundleIsHardError(t *testing.T) {
	cfg := config.Config{
		Profile:         config.ProfileEmbeddedLocal,
		CanvasBaseURL:   "http://127.0.0.1:4000/canvas",
		BlueBaseURL:     "http://127.0.0.1:4000/blue",
		SceneBundlePath: filepath.Join(t.TempDir(), "does-not-exist.json"),
	}
	if _, _, err := selectFetcher(cfg, tokenStub); err == nil {
		t.Fatal("expected hard error for a missing bundle path, got nil")
	}
}
