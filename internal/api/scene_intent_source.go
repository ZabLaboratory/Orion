package api

import "github.com/ZabLaboratory/Orion/internal/attestation"

// verifiedSceneArtifacts keeps the authoring source separate from the optional
// current-backend render capsule. Verification neither compiles nor loads Blue.
// Source integrity retains the existing envelope/delegation guarantee; unlike
// Program and CurrentRender, its transport digest is not a signed claim.
type verifiedSceneArtifacts struct {
	Program       []byte
	Source        []byte
	CurrentRender []byte
	BlueManifest  []byte
}

func verifySceneArtifacts(envelope resolvedSceneEnvelope, claims *attestation.Claims, cache *VerifiedProgramCache) (verifiedSceneArtifacts, error) {
	var artifacts verifiedSceneArtifacts
	var err error
	if claims.BlueProgramDigest == "" {
		err = verifyNoProgramEnvelopeValue(envelope)
	} else {
		artifacts.Program, err = decodeAndVerifyProgramEnvelopeCached(envelope, claims.BlueProgramDigest, cache)
	}
	if err != nil {
		return verifiedSceneArtifacts{}, err
	}
	artifacts.Source, err = decodeAndVerifyBundleEnvelope(envelope)
	if err != nil {
		return verifiedSceneArtifacts{}, err
	}
	artifacts.CurrentRender, err = decodeAndVerifyRenderBundleEnvelope(envelope, claims.RenderBundleDigest)
	if err != nil {
		return verifiedSceneArtifacts{}, err
	}
	artifacts.BlueManifest, err = verifySceneBlueManifest(envelope.BlueManifest, claims, artifacts.Source)
	if err != nil {
		return verifiedSceneArtifacts{}, err
	}
	return artifacts, nil
}
