package bluehost

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	blueruntime "github.com/ZabLaboratory/Blue/runtime/go"
	"github.com/ZabLaboratory/Orion/internal/effects"
)

type slotMirrorProbe struct {
	mu    sync.Mutex
	calls [][2]string
}

func (p *slotMirrorProbe) EmitSlotAssignment(slotRef, peerLabel string) {
	p.mu.Lock()
	p.calls = append(p.calls, [2]string{slotRef, peerLabel})
	p.mu.Unlock()
}

func TestAssignSlotHandlerPersistsThenMirrors(t *testing.T) {
	var gotPath, gotAuth string
	var gotPayload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotPayload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	mirror := &slotMirrorProbe{}
	handler := NewEffectHandlers(EffectDeps{
		ServiceCall: effects.NewServiceCallClient(server.URL, func(paths []string) string {
			if len(paths) != 1 || paths[0] != "zabcam.slots.assign" {
				t.Errorf("token paths = %#v", paths)
			}
			return "slot-token"
		}, nil),
		ResolveServiceRoute: func(service, routeID string) (ServiceCallRoute, bool) {
			if service != "zabcam" || routeID != "zabcam.slots.assign" {
				return ServiceCallRoute{}, false
			}
			return ServiceCallRoute{Service: service, RouteID: routeID, Method: http.MethodPut, PathTemplate: "/cam/api/v1/cam/streams/{stream_id}/slots/{slot_ref}", Params: []string{"stream_id", "slot_ref"}, TokenPaths: []string{routeID}}, true
		},
		EgressBudget:    effects.NewStreamEgressLimiter(2, 60),
		EgressBudgetKey: "live",
		StreamID:        "live",
		SlotMirror:      mirror,
	}, blueruntime.Execute)["zabcam.assign-slot@1"]

	outputs, err := handler(map[string]any{"service": "zabcam", "route_id": "zabcam.slots.assign"}, map[string]any{"slot_ref": "cam-0", "peer_label": "peer-0"})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if ok, _ := outputs["ok"].(bool); !ok {
		t.Fatalf("outputs = %#v", outputs)
	}
	if gotPath != "/cam/api/v1/cam/streams/live/slots/cam-0" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer slot-token" {
		t.Fatalf("authorization = %q", gotAuth)
	}
	if gotPayload["peer_label"] != "peer-0" {
		t.Fatalf("payload = %#v", gotPayload)
	}
	if len(mirror.calls) != 1 || mirror.calls[0] != [2]string{"cam-0", "peer-0"} {
		t.Fatalf("mirror calls = %#v", mirror.calls)
	}
}

func TestAssignSlotHandlerDoesNotMirrorNon2xx(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"detail":"peer not in room"}`))
	}))
	defer server.Close()
	mirror := &slotMirrorProbe{}
	handler := NewEffectHandlers(EffectDeps{
		ServiceCall: effects.NewServiceCallClient(server.URL, func([]string) string { return "slot-token" }, nil),
		ResolveServiceRoute: func(service, routeID string) (ServiceCallRoute, bool) {
			return ServiceCallRoute{Service: service, RouteID: routeID, Method: http.MethodPut, PathTemplate: "/slots/{stream_id}/{slot_ref}", Params: []string{"stream_id", "slot_ref"}, TokenPaths: []string{routeID}}, true
		},
		SlotMirror: mirror,
	}, blueruntime.Execute)["zabcam.assign-slot@1"]

	if _, err := handler(map[string]any{"service": "zabcam", "route_id": "zabcam.slots.assign"}, map[string]any{"slot_ref": "cam-0", "peer_label": "peer-0"}); err == nil {
		t.Fatal("non-2xx assignment unexpectedly succeeded")
	}
	if len(mirror.calls) != 0 {
		t.Fatalf("mirror calls = %#v", mirror.calls)
	}
}
