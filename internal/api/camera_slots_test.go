package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

type cameraSlotAssignerProbe struct {
	assignments map[string]string
	cleared     []string
}

type cameraSlotAssignerOnly struct{}

func (cameraSlotAssignerOnly) AssignCameraSlot(context.Context, string, string) error {
	return nil
}

type cameraSlotOptionalReleaseUnavailable struct {
	assignments map[string]string
}

func (p *cameraSlotOptionalReleaseUnavailable) AssignCameraSlot(
	_ context.Context,
	slotRef, peerLabel string,
) error {
	if p.assignments == nil {
		p.assignments = map[string]string{}
	}
	p.assignments[slotRef] = peerLabel
	return nil
}

func (cameraSlotOptionalReleaseUnavailable) ReleaseCameraSlot(context.Context, string) error {
	return fmt.Errorf("EGRESS_ROUTE_NOT_DECLARED: zabcam/zabcam.slots.release")
}

func (p *cameraSlotAssignerProbe) AssignCameraSlot(_ context.Context, slotRef, peerLabel string) error {
	if p.assignments == nil {
		p.assignments = map[string]string{}
	}
	p.assignments[slotRef] = peerLabel
	return nil
}

func (p *cameraSlotAssignerProbe) ReleaseCameraSlot(_ context.Context, slotRef string) error {
	p.cleared = append(p.cleared, slotRef)
	delete(p.assignments, slotRef)
	return nil
}

func cameraSlotRequest(body string, role string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/host/camera-slots", bytes.NewBufferString(body))
	req.Header.Set("X-Authenticated-User", "operator-1")
	req.Header.Set("X-Authenticated-Role", role)
	return req
}

func TestPostCameraSlotsProjectsThreeBoundedAssignments(t *testing.T) {
	probe := &cameraSlotAssignerProbe{}
	rec := httptest.NewRecorder()
	postCameraSlots(probe)(rec, cameraSlotRequest(`{
		"stream_id":"live",
		"assignments":[
			{"slot_ref":"cam-slot-0","peer_label":"fake-cam-1"},
			{"slot_ref":"cam-slot-1","peer_label":"fake-cam-2"},
			{"slot_ref":"cam-slot-2","peer_label":"fake-cam-3"}
		]
	}`, "operator"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !reflect.DeepEqual(probe.assignments, map[string]string{
		"cam-slot-0": "fake-cam-1",
		"cam-slot-1": "fake-cam-2",
		"cam-slot-2": "fake-cam-3",
	}) {
		t.Fatalf("projected assignments = %#v", probe.assignments)
	}
	var response cameraSlotProjectionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != "projected" || response.StreamID != "live" || len(response.Assignments) != 3 {
		t.Fatalf("response = %#v", response)
	}
}

func TestPostCameraSlotsRequiresOperatorAndThreeSlotLimit(t *testing.T) {
	probe := &cameraSlotAssignerProbe{}
	for _, test := range []struct {
		name string
		body string
		role string
		want int
	}{
		{
			name: "viewer",
			body: `{"assignments":[{"slot_ref":"cam-slot-0","peer_label":"cam"}]}`,
			role: "viewer",
			want: http.StatusForbidden,
		},
		{
			name: "four slots",
			body: `{"assignments":[{"slot_ref":"a","peer_label":"1"},{"slot_ref":"b","peer_label":"2"},{"slot_ref":"c","peer_label":"3"},{"slot_ref":"d","peer_label":"4"}]}`,
			role: "operator",
			want: http.StatusBadRequest,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			postCameraSlots(probe)(rec, cameraSlotRequest(test.body, test.role))
			if rec.Code != test.want {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestPostCameraSlotsReleasesStaleBindingsBeforeProjecting(t *testing.T) {
	probe := &cameraSlotAssignerProbe{assignments: map[string]string{"cam-slot-old": "stale"}}
	rec := httptest.NewRecorder()
	postCameraSlots(probe)(rec, cameraSlotRequest(`{
		"stream_id":"live",
		"clear_slot_refs":["cam-slot-old"],
		"assignments":[{"slot_ref":"cam-slot-0","peer_label":"fake-cam-1"}]
	}`, "operator"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !reflect.DeepEqual(probe.cleared, []string{"cam-slot-old"}) {
		t.Fatalf("cleared = %#v", probe.cleared)
	}
	if _, stale := probe.assignments["cam-slot-old"]; stale {
		t.Fatalf("stale assignment survived: %#v", probe.assignments)
	}
	var response cameraSlotProjectionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(response.Cleared, []string{"cam-slot-old"}) {
		t.Fatalf("response cleared = %#v", response.Cleared)
	}
}

func TestPostCameraSlotsKeepsCurrentAssignmentsWhenReleaseRouteIsMissing(t *testing.T) {
	probe := &cameraSlotOptionalReleaseUnavailable{}
	rec := httptest.NewRecorder()
	postCameraSlots(probe)(rec, cameraSlotRequest(`{
		"clear_slot_refs":["stale-slot"],
		"assignments":[{"slot_ref":"cam-slot-0","peer_label":"fake-cam-1"}]
	}`, "operator"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !reflect.DeepEqual(probe.assignments, map[string]string{"cam-slot-0": "fake-cam-1"}) {
		t.Fatalf("assignments = %#v", probe.assignments)
	}
	var response cameraSlotProjectionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Cleared) != 0 {
		t.Fatalf("cleared = %#v, want no acknowledged clear", response.Cleared)
	}
}

func TestPostCameraSlotsRejectsClearWithoutReleaser(t *testing.T) {
	probe := cameraSlotAssignerOnly{}
	rec := httptest.NewRecorder()
	postCameraSlots(probe)(rec, cameraSlotRequest(`{
		"clear_slot_refs":["cam-slot-old"],"assignments":[]
	}`, "operator"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}
