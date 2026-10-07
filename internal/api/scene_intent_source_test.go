package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/attestation"
	"github.com/ZabLaboratory/Orion/internal/canonical"
)

func TestCanvasManifestUsesSourceVersionDistinctFromArtifactSet(t *testing.T) {
	claims := &attestation.Claims{SceneID: "scene-1", RevisionID: "rev-1", ArtifactSetDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	version := "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	source := []byte(`{"lsml":"1.2","scene_id":"scene-1","scene_version":"` + version + `","layout":{"type":"frame","children":[]}}`)
	var manifest map[string]any
	decoder := json.NewDecoder(bytes.NewReader(manifestForTest(t, claims)))
	decoder.UseNumber()
	if err := decoder.Decode(&manifest); err != nil {
		t.Fatal(err)
	}
	manifest["scene_version"] = version
	delete(manifest, "manifest_digest")
	digest, err := canonical.Digest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifest["manifest_digest"] = digest
	raw, _ := json.Marshal(manifest)
	envelope := resolvedSceneEnvelope{BlueManifest: raw, lsmlBundleBytes: source, LSMLBundleDigest: sha256Digest(source)}
	if _, err := verifySceneArtifacts(envelope, claims, nil); err != nil {
		t.Fatalf("Canvas manifest must pin the LSML address, not the artifact-set digest: %v", err)
	}
	for _, field := range []string{"scene_id", "scene_version", "revision_id"} {
		t.Run(field, func(t *testing.T) {
			var changed map[string]any
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.UseNumber()
			if err := decoder.Decode(&changed); err != nil {
				t.Fatal(err)
			}
			changed[field] = "different"
			delete(changed, "manifest_digest")
			digest, err := canonical.Digest(changed)
			if err != nil {
				t.Fatal(err)
			}
			changed["manifest_digest"] = digest
			envelope.BlueManifest, _ = json.Marshal(changed)
			if _, err := verifySceneArtifacts(envelope, claims, nil); err == nil {
				t.Fatal("mismatched Canvas/source identity admitted")
			}
		})
	}
}

func TestVerifySceneArtifacts_KeepsSourceAndRenderSeparate(t *testing.T) {
	source := []byte(`{"lsml":"1.1","layout":{"kind":"text"},"defaults":{"label":"source"}}`)
	render := []byte(`{"root":{"kind":"text"},"defaults":{"label":"render"}}`)
	claims := &attestation.Claims{RenderBundleDigest: sha256Digest(render)}
	for _, local := range []bool{false, true} {
		envelope := resolvedSceneEnvelope{LSMLBundleDigest: sha256Digest(source), RenderBundleDigest: sha256Digest(render)}
		if local {
			envelope.lsmlBundleBytes, envelope.renderBundleBytes = source, render
		} else {
			envelope.LSMLBundle = base64.StdEncoding.EncodeToString(source)
			envelope.RenderBundle = base64.StdEncoding.EncodeToString(render)
		}
		artifacts, err := verifySceneArtifacts(envelope, claims, nil)
		if err != nil || !bytes.Equal(artifacts.Source, source) || !bytes.Equal(artifacts.CurrentRender, render) || artifacts.Program != nil {
			t.Fatalf("local=%t: artifacts=%+v err=%v", local, artifacts, err)
		}
		got, defaults, err := prepareCurrentRender(artifacts)
		if err != nil || !bytes.Equal(got, render) || string(defaults["label"]) != `"render"` || !bytes.Equal(artifacts.Source, source) {
			t.Fatalf("render adaptation: bundle=%s defaults=%v err=%v", got, defaults, err)
		}
	}
}

func TestVerifySceneArtifacts_SourceDoesNotRequireRenderCapsule(t *testing.T) {
	source := []byte(`{"lsml":"1.1","layout":{"kind":"text"}}`)
	artifacts, err := verifySceneArtifacts(resolvedSceneEnvelope{lsmlBundleBytes: source, LSMLBundleDigest: sha256Digest(source)}, &attestation.Claims{}, nil)
	if err != nil || !bytes.Equal(artifacts.Source, source) || artifacts.CurrentRender != nil {
		t.Fatalf("source-only verification: %+v %v", artifacts, err)
	}
}

func TestVerifySceneArtifacts_RetainsAdmissionFailures(t *testing.T) {
	source := []byte(`{"lsml":"1.1"}`)
	for name, envelope := range map[string]resolvedSceneEnvelope{
		"unattested program":    {BlueProgram: "e30="},
		"missing source digest": {lsmlBundleBytes: source},
		"bad source digest":     {lsmlBundleBytes: source, LSMLBundleDigest: sha256Digest([]byte("different"))},
		"unsigned render":       {renderBundleBytes: []byte(`{}`), RenderBundleDigest: sha256Digest([]byte(`{}`))},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := verifySceneArtifacts(envelope, &attestation.Claims{}, nil); err == nil {
				t.Fatal("invalid artifacts were admitted")
			}
		})
	}
}

func TestPrepareCurrentRender_DoesNotCompileSourceAtSceneSwitch(t *testing.T) {
	source := []byte(`{"lsml":"1.1"}`)
	got, state, err := prepareCurrentRender(verifiedSceneArtifacts{Source: source})
	if err != nil || got != nil || state != nil {
		t.Fatalf("source-only scene switch must not create a render bundle: bundle=%s defaults=%v err=%v", got, state, err)
	}
}

func TestPrepareCurrentRender_PreservesPassthroughAndRejectsBadDefaults(t *testing.T) {
	source := []byte(`{"lsml":"1.1"}`)
	got, defaults, err := prepareCurrentRender(verifiedSceneArtifacts{Source: source})
	if err != nil || got != nil || defaults != nil {
		t.Fatalf("source-only scene switch created a render bundle: %s %v %v", got, defaults, err)
	}
	_, _, err = prepareCurrentRender(verifiedSceneArtifacts{CurrentRender: []byte(`{"defaults":42}`)})
	if err == nil {
		t.Fatalf("invalid capsule defaults must retain integrity failure taxonomy: %v", err)
	}
}
