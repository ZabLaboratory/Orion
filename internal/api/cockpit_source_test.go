package api

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/attestation"
	"github.com/ZabLaboratory/Orion/internal/bluehost"
)

func TestCockpit_SourceOnlyIntentPreviewAndProgram(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	program := buildEngineBOperatorProgram(t, "league", "called", "pick", "core.primitive.integer", "picked")
	source := []byte(`{"lsml":"1.1","layout":{"kind":"text"},"operator_inputs":[{"path":"league","label":"League","type":"text","enum_values":["LEC","LCK"],"writable_by":["operator"]}]}`)
	envelope, digest := canvasEnvelopeWithBundle(program, source)
	deps, observed := noblueDeps(t, pub, envelope)
	t.Cleanup(func() {
		deps.Bridges.StopAll()
		_ = deps.Host.Release(bluehost.SlotPreview, "test")
		_ = deps.Host.Release(bluehost.SlotOnAir, "test")
	})
	ref := signedRef(t, priv, "canvas-key-1", attestation.ActionPreparePreview, time.Now(), "scene-1", digest)
	mux := http.NewServeMux()
	// Use the real scene-intent path and public contract routes, without a
	// compatibility bundle or compilation at any point.
	RegisterPublic(mux, PublicDeps{Logger: testLogger(), SceneIntent: &deps})
	// Avoid the legacy-rule compatibility lookup on this isolated fixture.
	f := &cockpitFixture{mux: mux}
	for _, target := range []struct {
		action      attestation.Action
		name, query string
		slot        bluehost.Slot
	}{
		{attestation.ActionPreparePreview, "preview", "&target=preview", bluehost.SlotPreview},
		{attestation.ActionTakeOnAir, "on-air", "", bluehost.SlotOnAir},
	} {
		if rec := sendIntent(t, deps, ref, target.action, target.name, "source-"+target.name); rec.Code != http.StatusOK {
			t.Fatalf("intent: %d %s", rec.Code, rec.Body.String())
		}
		// The initial Blue step is asynchronous behind the source snapshot.
		deadline := time.Now().Add(3 * time.Second)
		for len(deps.Host.PendingAwaitNames(target.slot)) == 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		_, body := getContracts(t, f, "operator", "?stream_id=show"+target.query)
		if len(body.Params) != 1 || body.Params[0].Path != "league" || len(body.Params[0].EnumValues) != 2 {
			t.Fatalf("source params: %+v", body.Params)
		}
		if len(body.Triggers) != 1 || body.Triggers[0].EntrypointID != "league" || len(body.Awaits) != 1 || body.Awaits[0].AwaitName != "pick" {
			t.Fatalf("source Blue contract: %+v", body)
		}
		if deps.Host.Bundle(target.slot) != nil {
			t.Fatal("operator contract recreated a compiled bundle")
		}
		if string(observed.source) != string(source) {
			t.Fatal("LSML source mutated")
		}
	}
	for _, query := range []struct {
		path   string
		status int
	}{
		{"/api/v1/scenes/scene-1/operator-inputs?v=" + deps.Host.Digest(bluehost.SlotOnAir), http.StatusOK},
		{"/api/v1/scenes/scene-1/operator-inputs?v=" + digest, http.StatusNotFound},
		{"/api/v1/scenes/scene-wrong/operator-inputs?v=" + digest, http.StatusNotFound},
		{"/api/v1/scenes/scene-1/operator-inputs?v=wrong", http.StatusNotFound},
		{"/api/v1/scenes/scene-1/operator-inputs", http.StatusNotFound},
	} {
		rec := opRequest(t, mux, "GET", query.path, "operator", nil)
		if rec.Code != query.status {
			t.Fatalf("%s: %d %s", query.path, rec.Code, rec.Body.String())
		}
		if rec.Code == http.StatusOK {
			var body struct {
				OperatorInputs []json.RawMessage `json:"operator_inputs"`
			}
			if json.Unmarshal(rec.Body.Bytes(), &body) != nil || len(body.OperatorInputs) != 1 {
				t.Fatalf("inputs: %s", rec.Body.String())
			}
		}
	}
}

func TestCockpit_PreviewUsesOwnBlueSlotAndEmptyStaysEmpty(t *testing.T) {
	live := buildEngineBOperatorProgram(t, "live-call", "live-called", "live-pick", "core.primitive.integer", "live-picked")
	preview := buildEngineBOperatorProgram(t, "preview-call", "preview-called", "preview-pick", "core.primitive.integer", "preview-picked")
	ef := newEngineBOperatorFixtureOnSlot(t, bluehost.SlotOnAir, live)
	f := &cockpitFixture{mux: ef.mux}
	_, empty := getContracts(t, f, "operator", "?stream_id=show&target=preview")
	if len(empty.Params)+len(empty.Triggers)+len(empty.Awaits) != 0 {
		t.Fatalf("empty Preview leaked: %+v", empty)
	}
	ef.takeSlot(t, bluehost.SlotPreview, preview)
	_, body := getContracts(t, f, "operator", "?stream_id=show&target=preview")
	if len(body.Triggers) != 1 || body.Triggers[0].EntrypointID != "preview-call" || len(body.Awaits) != 1 || body.Awaits[0].AwaitName != "preview-pick" {
		t.Fatalf("Preview contract: %+v", body)
	}
}
