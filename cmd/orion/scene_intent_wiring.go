package main

import (
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"

	"github.com/ZabLaboratory/Orion/internal/api"
	"github.com/ZabLaboratory/Orion/internal/attestation"
	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/config"
	"github.com/ZabLaboratory/Orion/internal/workload"
)

// wireSceneIntent builds the additive stateless-cutover surface (#331,
// ADR-BLUE-012). It returns (nil, nil) whenever the feature is not
// configured — WorkloadZabGateURL, the mTLS cert/key/CA, and at least
// one Canvas trust key are ALL required; any one missing leaves the
// route unregistered and every other boot behaviour byte-for-byte
// unchanged (Phase A of the #331 cutover plan posted on the issue).
func wireSceneIntent(cfg config.Config) (*api.SceneIntentDeps, error) {
	if cfg.WorkloadZabGateURL == "" ||
		cfg.WorkloadClientCertPath == "" || cfg.WorkloadClientKeyPath == "" || cfg.WorkloadCAPath == "" ||
		cfg.CanvasTrustPath == "" {
		return nil, nil
	}
	if cfg.WorkloadSAN == "" {
		return nil, fmt.Errorf("scene-intent: ORION_WORKLOAD_SAN is required once the workload surface is configured")
	}
	if cfg.OwnerID == "" || cfg.TenantID == "" {
		return nil, fmt.Errorf("scene-intent: ORION_OWNER_ID and ORION_TENANT_ID are required once the workload surface is configured")
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

	return &api.SceneIntentDeps{
		Trust:         trust,
		LocatorPrefix: cfg.CanvasLocatorPrefix,
		OwnerID:       cfg.OwnerID,
		TenantID:      cfg.TenantID,
		Workload:      wc,
		Host:          bluehost.NewHost(),
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
