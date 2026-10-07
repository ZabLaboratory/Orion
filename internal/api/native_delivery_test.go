package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/blueproject"
	"github.com/ZabLaboratory/Orion/internal/bluewire"
	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/obs"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

func TestNativeOperatorCallWaitsForPublicationAndRejectsACKFailure(t *testing.T) {
	host := bluehost.NewHost()
	if err := loadEngineBSlot(host, bluehost.SlotOnAir, buildEngineBOperatorProgram(t, "LEC", "called", "", "", "")); err != nil {
		t.Fatal(err)
	}
	defer host.Release(bluehost.SlotOnAir, "test-end")
	mirror := &recordingMirror{}
	bridges := bluewire.NewRegistry()
	bridges.Start(bluehost.SlotOnAir, bluewire.NewBridge(host, bluehost.SlotOnAir, mirror, "scene-1", "sha256:scene", "instance-1", blueproject.TargetProgram, "revision-1", "intent-1"), time.Hour, nil)
	defer bridges.StopAll()
	show := runtime.NewShow(runtime.NewComputeRegistry(), testLogger())
	defer show.Stop()
	flushed := false
	mux := http.NewServeMux()
	RegisterPublic(mux, PublicDeps{Logger: testLogger(), Metrics: obs.NewMetrics(), Show: show,
		SceneIntent: &SceneIntentDeps{Host: host, Bridges: bridges},
		NativeLSDPFlush: func(context.Context) error {
			flushed = true
			if mirror.count() != 1 {
				t.Fatal("receiver barrier overtook the operator result")
			}
			return errors.New("receiver unavailable")
		},
	})
	response := opRequest(t, mux, "POST", "/api/v1/operator/call/_/LEC", "operator", map[string]any{"payload": "native-lec"})
	if !flushed || response.Code != http.StatusServiceUnavailable {
		t.Fatalf("false operator acknowledgement: %d %s; flushed=%v", response.Code, response.Body.String(), flushed)
	}
}

func TestNativeEditableACKWaitsForScenePublication(t *testing.T) {
	preview := runtime.NewPreviewSlot(context.Background(), runtime.NewComputeRegistry(), &editableAPIWire{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer preview.Close()
	preview.ActivateEditableWithBundle("editable", &compiler.Graph{SceneID: "editable", SceneVersion: "v1", Defaults: map[string]json.RawMessage{"__editable.node.x": json.RawMessage(`10`)}}, &compiler.RenderBundle{}, 0, []byte(`{}`))
	called := false
	deps := PublicDeps{Preview: preview, NativeLSDPFlush: func(context.Context) error {
		_, _, state, ok := preview.SnapshotState()
		if !ok || string(state["__editable.node.x"]) != "42" {
			t.Fatal("native producer flushed before the scene applied the input")
		}
		called = true
		return errors.New("receiver unavailable")
	}}
	var body editablePreviewPatchRequest
	json.Unmarshal([]byte(`{"scene_id":"editable","base_edit_seq":0,"edit_seq":1,"patches":[{"path":"__editable.node.x","value":42}]}`), &body)
	status, code, err := applyEditablePreviewPatch(context.Background(), deps, body)
	if !called || err == nil || status != http.StatusServiceUnavailable || code != "NATIVE_LSDP_DELIVERY_FAILED" {
		t.Fatalf("false acknowledgement: %d %s %v; flushed=%v", status, code, err, called)
	}
}

func TestNativeCameraACKFailureIsVisible(t *testing.T) {
	probe := &cameraSlotAssignerProbe{}
	rec := httptest.NewRecorder()
	postCameraSlots(probe, func(context.Context) error {
		if probe.assignments["slot"] != "peer" {
			t.Fatal("native flush overtook durable camera assignment")
		}
		return errors.New("receiver unavailable")
	})(rec, cameraSlotRequest(`{"assignments":[{"slot_ref":"slot","peer_label":"peer"}]}`, "operator"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("camera delivery failure acknowledged: %d %s", rec.Code, rec.Body.String())
	}
}
