package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/config"
)

// writeSelfSignedCert generates a throwaway Ed25519 leaf cert + key,
// PEM-encodes both, and writes them beside a CA file containing the same
// cert (self-signed ⇒ it is its own trust anchor) — enough for
// wireSceneIntent to load a valid tls.Certificate + x509.CertPool without
// a real ZabAuth-issued workload identity.
func writeSelfSignedCert(t *testing.T, dir string) (certPath, keyPath, caPath string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "orion-workload-test"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	caPath = filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath, caPath
}

func writeCanvasTrust(t *testing.T, dir string) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	trust := map[string]string{"canvas-key-1": base64.StdEncoding.EncodeToString(pub)}
	raw, _ := json.Marshal(trust)
	path := filepath.Join(dir, "canvas-trust.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWireSceneIntent_DarkByDefault(t *testing.T) {
	deps, err := wireSceneIntent(config.Config{})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if deps != nil {
		t.Fatal("expected nil deps when workload surface is not configured")
	}
}

func TestWireSceneIntent_FullyConfigured(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, caPath := writeSelfSignedCert(t, dir)
	trustPath := writeCanvasTrust(t, dir)

	cfg := config.Config{
		WorkloadZabGateURL:     "https://zabgate.internal",
		WorkloadClientCertPath: certPath,
		WorkloadClientKeyPath:  keyPath,
		WorkloadCAPath:         caPath,
		WorkloadSAN:            "spiffe://zab/workload/orion/dev/instance-1",
		CanvasTrustPath:        trustPath,
		CanvasLocatorPrefix:    "scenes/",
		OwnerID:                "owner-1",
		TenantID:               "tenant-1",
	}

	deps, err := wireSceneIntent(cfg)
	if err != nil {
		t.Fatalf("wireSceneIntent: %v", err)
	}
	if deps == nil {
		t.Fatal("expected non-nil deps")
	}
	if len(deps.Trust) != 1 {
		t.Fatalf("expected 1 trust key, got %d", len(deps.Trust))
	}
	if deps.Host == nil || deps.Workload == nil {
		t.Fatal("expected Host and Workload to be wired")
	}
}

func TestWireSceneIntent_MissingSANFailsClosed(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, caPath := writeSelfSignedCert(t, dir)
	trustPath := writeCanvasTrust(t, dir)

	cfg := config.Config{
		WorkloadZabGateURL:     "https://zabgate.internal",
		WorkloadClientCertPath: certPath,
		WorkloadClientKeyPath:  keyPath,
		WorkloadCAPath:         caPath,
		CanvasTrustPath:        trustPath,
		OwnerID:                "owner-1",
		TenantID:               "tenant-1",
	}

	if _, err := wireSceneIntent(cfg); err == nil {
		t.Fatal("expected error for missing ORION_WORKLOAD_SAN")
	}
}

func TestWireSceneIntent_MissingOwnerTenantFailsClosed(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, caPath := writeSelfSignedCert(t, dir)
	trustPath := writeCanvasTrust(t, dir)

	cfg := config.Config{
		WorkloadZabGateURL:     "https://zabgate.internal",
		WorkloadClientCertPath: certPath,
		WorkloadClientKeyPath:  keyPath,
		WorkloadCAPath:         caPath,
		WorkloadSAN:            "spiffe://zab/workload/orion/dev/instance-1",
		CanvasTrustPath:        trustPath,
	}

	if _, err := wireSceneIntent(cfg); err == nil {
		t.Fatal("expected error for missing owner/tenant")
	}
}
