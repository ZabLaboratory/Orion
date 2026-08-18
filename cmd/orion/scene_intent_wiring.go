package main

import (
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/ZabLaboratory/Orion/internal/api"
	"github.com/ZabLaboratory/Orion/internal/attestation"
	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/config"
	"github.com/ZabLaboratory/Orion/internal/providers"
	"github.com/ZabLaboratory/Orion/internal/workload"
)

// wireSceneIntent builds the additive stateless-cutover surface (#331,
// ADR-BLUE-012). It returns (nil, nil) only when NONE of the workload
// vars are set — the intentional, fully-dark default. If ANY one of
// them is set but not every required one, that is an operator mistake
// (a half-finished rollout, a typo'd env name), not "feature off" — it
// returns an error instead of silently staying dark, so a misconfigured
// deploy is visible at boot rather than a route that quietly never
// registers. Phase A of the #331 cutover plan posted on the issue.
func wireSceneIntent(cfg config.Config, logger *slog.Logger, effectDeps bluehost.EffectDeps) (*api.SceneIntentDeps, error) {
	required := map[string]string{
		"ORION_WORKLOAD_ZABGATE_URL":      cfg.WorkloadZabGateURL,
		"ORION_WORKLOAD_CLIENT_CERT_PATH": cfg.WorkloadClientCertPath,
		"ORION_WORKLOAD_CLIENT_KEY_PATH":  cfg.WorkloadClientKeyPath,
		"ORION_WORKLOAD_CA_PATH":          cfg.WorkloadCAPath,
		"ORION_CANVAS_TRUST_PATH":         cfg.CanvasTrustPath,
		"ORION_WORKLOAD_SAN":              cfg.WorkloadSAN,
		"ORION_OWNER_ID":                  cfg.OwnerID,
		"ORION_TENANT_ID":                 cfg.TenantID,
	}
	present, missing := 0, []string{}
	for name, v := range required {
		if v == "" {
			missing = append(missing, name)
		} else {
			present++
		}
	}
	if present == 0 {
		return nil, nil // fully dark: intentional feature-off
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("scene-intent: partially configured (%d/%d vars set) — missing %v; set all of them or none", present, len(required), missing)
	}

	cert, err := tls.LoadX509KeyPair(cfg.WorkloadClientCertPath, cfg.WorkloadClientKeyPath)
	if err != nil {
		return nil, fmt.Errorf("scene-intent: load workload mTLS keypair: %w", err)
	}
	caPEM, err := os.ReadFile(cfg.WorkloadCAPath)
	if err != nil {
		return nil, fmt.Errorf("scene-intent: read workload CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("scene-intent: workload CA file carries no usable certificate")
	}

	httpClient := workload.NewMTLSHTTPClient(cert, roots)
	identity := workload.Identity{San: cfg.WorkloadSAN}
	if len(cert.Certificate) > 0 {
		identity.CertSHA256 = workload.FingerprintCert(cert.Certificate[0])
	}
	wc, err := workload.NewClient(cfg.WorkloadZabGateURL, identity, httpClient)
	if err != nil {
		return nil, fmt.Errorf("scene-intent: build workload client: %w", err)
	}

	trust, err := loadCanvasTrust(cfg.CanvasTrustPath)
	if err != nil {
		return nil, err
	}
	if len(trust) == 0 {
		return nil, fmt.Errorf("scene-intent: %s carries no trust keys", cfg.CanvasTrustPath)
	}

	httpEgressAllowed := len(cfg.HTTPEgressAllowHosts) > 0

	// Wire the async `core.effect.invoke@1` invocation/completion protocol
	// (Blue PR #313, Orion #336) to the same transport dependencies that
	// Engine A's SceneEffects uses. The host retains Runtime.Complete for
	// this generic protocol; its direct EffectHandlers are configured from
	// the same bundle when Prepare/Take runs.
	host := bluehost.NewHost()
	host.SetHTTPEffects(effectDeps, logger)
	// core.overlay-app.set@1's real effector (ENGINE-B-PARITY-ORION,
	// internal/bluehost/effect_overlay.go) — nil-safe when effectDeps.
	// OverlayMirror was never set (bespoke mode / no antenne LSDP wire).
	host.SetOverlayMirror(effectDeps.OverlayMirror)
	assetBaseURL := strings.TrimRight(cfg.CanvasBaseURL, "/") + "/api/v1/scene-assets"
	// CanvasBaseURL is the container-local gateway address in production.
	// Solar runs in Prism, so content-addressed assets must use the public
	// gateway origin when one is configured; otherwise the browser cannot
	// reach the internal Docker hostname and the host allowlist is wrong too.
	publicBaseURL := strings.TrimRight(cfg.PublicBaseURL, "/")
	if strings.HasSuffix(publicBaseURL, "/orion") {
		assetBaseURL = strings.TrimSuffix(publicBaseURL, "/orion") + "/canvas/api/v1/scene-assets"
	}

	return &api.SceneIntentDeps{
		Trust:         trust,
		LocatorPrefix: cfg.CanvasLocatorPrefix,
		OwnerID:       cfg.OwnerID,
		TenantID:      cfg.TenantID,
		Workload:      wc,
		Host:          host,
		StaticBundleCompiler: func(raw []byte, sceneID, sceneVersion string) ([]byte, map[string]json.RawMessage, error) {
			return compiler.CompileStaticLSML(raw, sceneID, sceneVersion, assetBaseURL)
		},
		Providers: providers.Registry(),
		Policy:    providers.Policy(httpEgressAllowed),
		Effects:   effectDeps,
		// Same budget Engine A's /validate/simulate harness runs under
		// (runtime.NewHarness / cfg.ValidationMaxSteps/MaxWall below) — one
		// operator-facing knob pair for "how long may a validation run",
		// not a second one invented for Engine B.
		ValidationMaxSteps: cfg.ValidationMaxSteps,
		ValidationMaxWall:  cfg.ValidationMaxWall,
	}, nil
}

// loadCanvasTrust parses a `{"<kid>": "<base64 Ed25519 pubkey>"}` file —
// ZabCanvas's published resolved-scene-ref signing keys (§6.2). Rotation
// is a file update on the deployment substrate; Verify never fetches a
// key from a URL carried by the attestation itself.
func loadCanvasTrust(path string) (attestation.TrustSet, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("scene-intent: read canvas trust: %w", err)
	}
	var encoded map[string]string
	if err := json.Unmarshal(raw, &encoded); err != nil {
		return nil, fmt.Errorf("scene-intent: parse canvas trust: %w", err)
	}
	trust := make(attestation.TrustSet, len(encoded))
	for kid, b64 := range encoded {
		key, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("scene-intent: canvas trust key %q: %w", kid, err)
		}
		if len(key) != 32 {
			return nil, fmt.Errorf("scene-intent: canvas trust key %q: expected 32-byte Ed25519 public key, got %d", kid, len(key))
		}
		trust[kid] = ed25519.PublicKey(key)
	}
	return trust, nil
}
