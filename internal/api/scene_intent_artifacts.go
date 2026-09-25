package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ZabLaboratory/Orion/internal/attestation"
	"github.com/ZabLaboratory/Orion/internal/canonical"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// resolvedSceneEnvelope is the slice of `zabcanvas.resolved-scene.v1`
// (§6.3) this handler consumes: the pinned blue.program.v1 bytes,
// base64-encoded, plus the digest Canvas computed over them at
// publication. Every other §6.3 field (LSML render-bundle, projection
// resources, full attestation echo) is out of this handler's scope.
type resolvedSceneEnvelope struct {
	BlueProgram       string `json:"blue_program"`
	BlueProgramDigest string `json:"blue_program_digest"`
	// LSMLBundle is OPTIONAL — an envelope with no bundle (e.g. an
	// operator-only rule with nothing to render) is valid;
	// decodeAndVerifyBundle returns (nil, nil) for it. LSMLBundleDigest is
	// MANDATORY the moment LSMLBundle is present (Bastion C4, PR #346,
	// fail-closed) — decodeAndVerifyBundle refuses an envelope that
	// carries a bundle with no digest, rather than skip verification.
	LSMLBundle       string `json:"lsml_bundle,omitempty"`
	LSMLBundleDigest string `json:"lsml_bundle_digest,omitempty"`
	// RenderBundle is compiled during Canvas validation. Its digest is also
	// signed in the Canvas ref claims; when present, scene-intent loads these
	// bytes verbatim and skips StaticBundleCompiler.
	RenderBundle       string `json:"render_bundle,omitempty"`
	RenderBundleDigest string `json:"render_bundle_digest,omitempty"`

	// Embedded-local activations keep artifact files as bytes instead of
	// converting them to the wire envelope's base64 strings and back.
	blueProgramBytes  []byte `json:"-"`
	lsmlBundleBytes   []byte `json:"-"`
	renderBundleBytes []byte `json:"-"`
}

type localSceneIndex struct {
	SceneID          string `json:"scene_id"`
	RevisionID       string `json:"revision_id"`
	LSMLBundleDigest string `json:"lsml_bundle_digest,omitempty"`
}

const maxLocalSceneArtifactBytes = 1 << 30

func loadLocalSceneEnvelope(root string, claims *attestation.Claims) (resolvedSceneEnvelope, error) {
	if root == "" {
		return resolvedSceneEnvelope{}, errors.New("local artifact root is not configured")
	}
	if !safeLocalComponent(claims.SceneID) || !safeLocalComponent(claims.RevisionID) {
		return resolvedSceneEnvelope{}, errors.New("scene or revision id is not a safe local cache key")
	}
	indexPath := filepath.Join(root, "scene-index", claims.SceneID+"--"+claims.RevisionID+".json")
	indexRaw, err := os.ReadFile(indexPath)
	if err != nil {
		return resolvedSceneEnvelope{}, fmt.Errorf("read local scene index: %w", err)
	}
	var index localSceneIndex
	if err := json.Unmarshal(indexRaw, &index); err != nil || index.SceneID != claims.SceneID || index.RevisionID != claims.RevisionID {
		return resolvedSceneEnvelope{}, errors.New("local scene index does not match the attested scene")
	}
	envelope := resolvedSceneEnvelope{}
	if claims.BlueProgramDigest != "" {
		program, err := readLocalArtifact(root, claims.BlueProgramDigest)
		if err != nil {
			return resolvedSceneEnvelope{}, fmt.Errorf("read local Blue program: %w", err)
		}
		envelope.blueProgramBytes = program
		envelope.BlueProgramDigest = claims.BlueProgramDigest
	}
	if index.LSMLBundleDigest != "" {
		bundle, err := readLocalArtifact(root, index.LSMLBundleDigest)
		if err != nil {
			return resolvedSceneEnvelope{}, fmt.Errorf("read local LSML bundle: %w", err)
		}
		envelope.lsmlBundleBytes = bundle
		envelope.LSMLBundleDigest = index.LSMLBundleDigest
	}
	if claims.RenderBundleDigest != "" {
		render, err := readLocalArtifact(root, claims.RenderBundleDigest)
		if err != nil {
			return resolvedSceneEnvelope{}, fmt.Errorf("read local render bundle: %w", err)
		}
		envelope.renderBundleBytes = render
		envelope.RenderBundleDigest = claims.RenderBundleDigest
	}
	return envelope, nil
}

func readLocalArtifact(root, digest string) ([]byte, error) {
	if len(digest) != len("sha256:")+64 || !strings.HasPrefix(digest, "sha256:") || !isHexDigest(digest[len("sha256:"):]) {
		return nil, errors.New("invalid local artifact digest")
	}
	path := filepath.Join(root, "artifacts", digest[len("sha256:"):]+".bin")
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() <= 0 || info.Size() > maxLocalSceneArtifactBytes {
		return nil, errors.New("local artifact size is outside the allowed range")
	}
	data := make([]byte, int(info.Size()))
	if _, err := io.ReadFull(file, data); err != nil {
		return nil, fmt.Errorf("read local artifact: %w", err)
	}
	var extra [1]byte
	if n, err := file.Read(extra[:]); n != 0 {
		return nil, errors.New("local scene artifact changed while reading")
	} else if err != io.EOF {
		if err != nil {
			return nil, err
		}
		return nil, io.ErrNoProgress
	}
	return data, nil
}

func safeLocalComponent(value string) bool {
	return value != "" && value != "." && value != ".." && filepath.Base(value) == value
}

func isHexDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

// decodeAndVerifyProgramEnvelopeCached decodes the pinned program bytes and
// cross-checks both the envelope digest and freshly computed canonical digest
// against the signed attestation claim. A mismatch fails closed before Load.
func decodeAndVerifyProgramEnvelopeCached(envelope resolvedSceneEnvelope, expectedDigest string, cache *VerifiedProgramCache) ([]byte, error) {
	if envelope.BlueProgramDigest != expectedDigest {
		return nil, errors.New("scene-intent: envelope blue_program_digest does not match the attested claim")
	}
	program, err := decodeSceneArtifact(envelope.BlueProgram, envelope.blueProgramBytes)
	if err != nil {
		return nil, err
	}
	if cache.contains(expectedDigest, program) {
		return program, nil
	}
	computedDigest, err := blueProgramDigest(program)
	if err != nil {
		return nil, err
	}
	if computedDigest != expectedDigest {
		return nil, errors.New("scene-intent: computed canonical program digest does not match the attested claim")
	}
	cache.add(expectedDigest, program)
	return program, nil
}

// blueProgramDigest verifies Blue's self-excluding program_digest contract.
// The digest is over the canonical JSON document with only program_digest
// removed, not over the raw JSON bytes that carry the self-referential field.
// Blue, ZabCanvas and the portable runtime all use this domain.
func blueProgramDigest(program []byte) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(program))
	decoder.UseNumber()
	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		return "", err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return "", errors.New("scene-intent: program contains multiple JSON values")
		}
		return "", err
	}
	claimedDigest, ok := document["program_digest"].(string)
	if !ok || claimedDigest == "" {
		return "", errors.New("scene-intent: program is missing program_digest")
	}
	delete(document, "program_digest")
	computedDigest, err := canonical.Digest(document)
	if err != nil {
		return "", err
	}
	if computedDigest != claimedDigest {
		return "", errors.New("scene-intent: program_digest does not match canonical program content")
	}
	return computedDigest, nil
}

// decodeAndVerifyBundle extracts the OPTIONAL LSML render-bundle from
// the Canvas artifact envelope. Unlike decodeAndVerifyProgram, there is
// no SIGNED claim to cross-check against (§6.2's claim set has no
// lsml_bundle_digest) — only the envelope's own self-consistency
// (declared digest == sha256 of the decoded bytes) is verified. Returns
// (nil, nil) when the envelope carries no bundle at all.
//
// Fail-closed (Bastion C4, PR #346): lsml_bundle_digest is MANDATORY once
// lsml_bundle is present. An envelope with a bundle but no digest is
// refused outright rather than served unverified — the prior fail-open
// (`if digest != ""`) let an unverified bundle ride all the way to
// GET /host/render-bundle.
func decodeAndVerifyBundle(body json.RawMessage) ([]byte, error) {
	var envelope resolvedSceneEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, err
	}
	return decodeAndVerifyBundleEnvelope(envelope)
}

func decodeAndVerifyBundleEnvelope(envelope resolvedSceneEnvelope) ([]byte, error) {
	if envelope.LSMLBundle == "" && envelope.lsmlBundleBytes == nil {
		return nil, nil
	}
	if envelope.LSMLBundleDigest == "" {
		return nil, errors.New("scene-intent: lsml_bundle present without lsml_bundle_digest")
	}
	bundle, err := decodeSceneArtifact(envelope.LSMLBundle, envelope.lsmlBundleBytes)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(bundle)
	if "sha256:"+hex.EncodeToString(sum[:]) != envelope.LSMLBundleDigest {
		return nil, errors.New("scene-intent: lsml_bundle does not match its own declared digest")
	}
	return bundle, nil
}

// decodeAndVerifyRenderBundleEnvelope extracts the optional Solar bundle
// produced by POST /validate/render-bundle. The expected digest comes from
// signed Canvas claims, so the envelope cannot substitute another artifact.
func decodeAndVerifyRenderBundleEnvelope(envelope resolvedSceneEnvelope, expectedDigest string) ([]byte, error) {
	if envelope.RenderBundle == "" && envelope.renderBundleBytes == nil {
		if expectedDigest != "" {
			return nil, errors.New("scene-intent: signed render_bundle_digest has no render_bundle")
		}
		return nil, nil
	}
	if expectedDigest == "" || envelope.RenderBundleDigest == "" || envelope.RenderBundleDigest != expectedDigest {
		return nil, errors.New("scene-intent: render bundle digest is not signed consistently")
	}
	bundle, err := decodeSceneArtifact(envelope.RenderBundle, envelope.renderBundleBytes)
	if err != nil || len(bundle) == 0 || !json.Valid(bundle) {
		return nil, errors.New("scene-intent: render bundle is not valid base64 JSON")
	}
	sum := sha256.Sum256(bundle)
	computed := "sha256:" + hex.EncodeToString(sum[:])
	if computed != envelope.RenderBundleDigest {
		return nil, errors.New("scene-intent: render bundle digest does not match bytes")
	}
	return bundle, nil
}

func decodeRenderBundleDefaults(bundle []byte) (map[string]json.RawMessage, error) {
	var payload struct {
		Defaults map[string]json.RawMessage `json:"defaults"`
	}
	if err := json.Unmarshal(bundle, &payload); err != nil {
		return nil, err
	}
	return payload.Defaults, nil
}

// verifyNoProgramEnvelopeValue enforces the no-program contract on the
// decoded envelope: bytes absent from signed claims must not enter Orion.
func verifyNoProgramEnvelopeValue(envelope resolvedSceneEnvelope) error {
	if envelope.BlueProgram != "" || envelope.BlueProgramDigest != "" || envelope.blueProgramBytes != nil {
		return errors.New("scene-intent: envelope carries a program the attestation did not sign")
	}
	return nil
}

func decodeSceneArtifact(encoded string, localBytes []byte) ([]byte, error) {
	if localBytes != nil {
		if encoded != "" {
			return nil, errors.New("scene-intent: artifact is present in both encoded and local form")
		}
		return localBytes, nil
	}
	return base64.StdEncoding.DecodeString(encoded)
}

// getHostRenderBundle serves the LSML render-bundle bytes attached to a
// slot by the most recent Prepare/Take (§15 read-route migration: the
// new-path equivalent of legacy's GET /scenes/{id}/render-bundle,
// content-addressed and immutably cacheable the same way — but keyed by
// slot, not scene_id, since the new model has no persisted roster to
// address by id). ?slot=preview|on-air, default preview.
