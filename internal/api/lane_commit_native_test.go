package api

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/bluewire"
	"github.com/ZabLaboratory/Orion/internal/canonical"
	"github.com/ZabLaboratory/Orion/internal/config"
	"github.com/ZabLaboratory/Orion/internal/lsdpreception"
	"github.com/ZabLaboratory/Orion/internal/protocol"
	"github.com/ZabLaboratory/Orion/internal/runtime"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The launcher owns a disposable native Rust receiver. Never point this test at
// the shared Prism node: all resources are explicitly synthetic fixtures.
func TestLaneCommitNative(t *testing.T) {
	address := os.Getenv("ORION_TEST_PRIVATE_LANE_ADDRESS")
	if address == "" {
		t.Skip("requires private native receiver")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	r, err := lsdpreception.New(address, "orion/state")
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Check(ctx); err != nil || !r.PhysicalLanes.Load() {
		t.Fatal("physical resources unavailable", err)
	}
	p := lsdpreception.NewProducer(ctx, r)
	defer p.Close()
	h := lsdpreception.NewHub(p, slog.Default())
	h.EnablePhysicalLanes()
	source := func(id string) []byte {
		return []byte(`{"lsml":"1.2","scene_id":"` + id + `","scene_version":"source-v1","layout":{"type":"text","text":{"$bind":"score"}},"defaults":{"score":0}}`)
	}
	program, preview := h.Lane("program"), h.Lane("preview")
	program.MirrorForLSML("scene-a", "artifact-a", source("scene-a"))
	b := preview.MirrorForLSML("scene-b", "artifact-b", source("scene-b"))
	program.SetActive("scene-a")
	preview.SetActive("scene-b")
	if err = p.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	aBefore, _ := r.Read(ctx, "solar/lane-a")
	bBefore, _ := r.Read(ctx, "solar/lane-b")
	host := bluehost.NewHost()
	fixture, err := os.ReadFile("../bluespike/testdata/01-minimal.program.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = host.Prepare(bluehost.SlotPreview, "actor-b", "scene-b", "digest-b", fixture, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err = host.Take("actor-a", "scene-a", "digest-a", fixture, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	_, _ = host.Step(bluehost.SlotPreview)
	_, _ = host.Step(bluehost.SlotOnAir)
	bridges := bluewire.NewRegistry()
	defer bridges.StopAll()
	previewSlot := runtime.NewPreviewSlot(ctx, runtime.NewComputeRegistry(), preview, slog.Default())
	previewSlot.SetEditableProgramWire(program)
	defer previewSlot.Close()
	d := NewSceneControlDocument()
	aSel := SceneSelection{"scene-a", "artifact-a", "live"}
	bSel := SceneSelection{"scene-b", "artifact-b", "live"}
	d.Defaults.Desired = map[string]SceneSelection{"program": aSel, "preview": bSel}
	d.Defaults.Observed = map[string]any{"program": map[string]any{"scene_id": "scene-a", "status": "active"}, "preview": map[string]any{"scene_id": "scene-b", "status": "active"}}
	if err = r.WriteLeaf(ctx, "orion/state", "/scene_control", d); err != nil {
		t.Fatal(err)
	}
	c := &SceneControl{deps: SceneIntentDeps{CommitLaneRoles: NativeLaneCommitter(h, host, bridges, previewSlot)}, reception: r, path: filepath.Join(t.TempDir(), "selection.lsml"), desired: d.Defaults.Desired, current: map[string]string{"program": selectionKey(aSel), "preview": selectionKey(bSel)}}
	done := make(chan struct{})
	go func() { defer close(done); _ = c.Run(ctx) }()
	defer func() { cancel(); <-done }()
	commit := lsdpreception.LaneCommit{Schema: "orion.lane-commit.v1", RuntimeInstanceID: "private-native", PrepareCommandID: "prepare-1", TakeCommandID: "take-1", IntentID: "intent-1", FrameID: 12, PtsNs: 1234, ServerSeq: 4, PreviousRoleMap: lsdpreception.RoleMap{OnAir: "A", Preview: "B"}, RoleMap: lsdpreception.RoleMap{OnAir: "B", Preview: "A"}, PreviousRevisions: lsdpreception.LaneRevisions{Program: 0, Preview: 1, RoleMap: 0}, Revisions: lsdpreception.LaneRevisions{Program: 1, Preview: 1, RoleMap: 1}, SceneID: "scene-b", SceneVersion: "source-v1"}
	if err = r.WriteLeaf(ctx, "orion/state", "/scene_control/defaults/lane_commit", commit); err != nil {
		t.Fatal(err)
	}
	await := func(take, status string) lsdpreception.LaneCommitReceipt {
		for ctx.Err() == nil {
			state, _ := r.Read(ctx, "orion/state")
			root, _ := state.(map[string]any)
			raw, _ := json.Marshal(root["scene_control"])
			var control SceneControlDocument
			_ = json.Unmarshal(raw, &control)
			raw, _ = json.Marshal(control.Defaults.Observed["lane_commit"])
			var receipt lsdpreception.LaneCommitReceipt
			_ = json.Unmarshal(raw, &receipt)
			if receipt.TakeCommandID == take && receipt.Status == status {
				return receipt
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("receipt missing")
		return lsdpreception.LaneCommitReceipt{}
	}
	firstReceipt := await("take-1", "committed")
	if receipt := firstReceipt; receipt.Targets["program"] != "solar/lane-b" {
		t.Fatal(receipt)
	}
	aAfter, _ := r.Read(ctx, "solar/lane-a")
	bAfter, _ := r.Read(ctx, "solar/lane-b")
	x, _ := json.Marshal(aBefore)
	y, _ := json.Marshal(aAfter)
	u, _ := json.Marshal(bBefore)
	v, _ := json.Marshal(bAfter)
	if string(x) != string(y) || string(u) != string(v) {
		t.Fatal("native document changed at role commit")
	}
	if !host.Serving(bluehost.SlotOnAir, "scene-b", "digest-b") || !host.Serving(bluehost.SlotPreview, "scene-a", "digest-a") {
		t.Fatal("Blue authority did not follow native roles")
	}
	c.mu.Lock()
	if c.desired["program"] != bSel || c.current["program"] != selectionKey(bSel) {
		t.Fatal("logical selections not reconciled")
	}
	c.mu.Unlock()
	// A new Preview document and late output from the retained B actor remain on
	// their respective physical resources after the permutation.
	// The real editable API replaces the former Program Blue actor on Preview.
	editable := map[string]any{"lsml": "1.2", "scene_id": "scene-c", "scene_version": "sha256:" + strings.Repeat("0", 64), "layout": map[string]any{"kind": "shape", "bind": map[string]any{"width": "__editable.width"}}, "defaults": map[string]any{"__editable.width": json.Number("10")}}
	editableVersion, err := canonical.Digest(editable)
	if err != nil {
		t.Fatal(err)
	}
	editable["scene_version"] = editableVersion
	mux := http.NewServeMux()
	RegisterPublic(mux, PublicDeps{Logger: slog.Default(), Config: config.Config{Profile: config.ProfileEmbeddedLocal}, Preview: previewSlot, SceneIntent: &SceneIntentDeps{Host: host, Bridges: bridges}, NativeLSDPFlush: p.Flush})
	opened := editableAPIRequest(t, mux, http.MethodPost, map[string]any{"scene_id": "scene-c", "scene_version": editableVersion, "render_mode": "source-only", "lsml_bundle": editable}, "operator")
	if opened.Code != http.StatusOK || !host.Serving(bluehost.SlotPreview, "scene-c", editableVersion) {
		t.Fatal("real editable admission failed", opened.Code, opened.Body.String())
	}
	b.Forward(&protocol.Delta{Patches: []protocol.Patch{{Path: "score", Value: json.RawMessage(`42`)}}})
	if err = p.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	state, _ := r.Read(ctx, "solar/lane-b")
	doc := state.(map[string]any)
	if doc["scene_id"] != "scene-b" || doc["defaults"].(map[string]any)["score"] != float64(42) {
		t.Fatal("new Preview touched Program", doc)
	}
	cBefore, _ := r.Read(ctx, "solar/lane-a")
	second := commit
	second.PrepareCommandID = "prepare-2"
	second.TakeCommandID = "take-2"
	second.IntentID = "intent-2"
	second.FrameID = 13
	second.PtsNs = 1235
	second.ServerSeq = 8
	second.PreviousRoleMap = commit.RoleMap
	second.RoleMap = commit.PreviousRoleMap
	second.PreviousRevisions = lsdpreception.LaneRevisions{Program: 1, Preview: 2, RoleMap: 1}
	second.Revisions = lsdpreception.LaneRevisions{Program: 2, Preview: 2, RoleMap: 2}
	second.SceneID = "scene-c"
	second.SceneVersion = editableVersion
	if err = r.WriteLeaf(ctx, "orion/state", "/scene_control/defaults/lane_commit", second); err != nil {
		t.Fatal(err)
	}
	receipt := await("take-2", "committed")
	if receipt.Sources["program"].SceneID != "scene-c" || receipt.Sources["program"].SceneVersion != editableVersion || !host.Serving(bluehost.SlotOnAir, "scene-c", editableVersion) || !host.Serving(bluehost.SlotPreview, "scene-b", "digest-b") {
		t.Fatal("Blue to editable role authority mismatch", receipt)
	}
	cAfter, _ := r.Read(ctx, "solar/lane-a")
	x, _ = json.Marshal(cBefore)
	y, _ = json.Marshal(cAfter)
	if string(x) != string(y) {
		t.Fatal("editable source changed at Take")
	}
	if _, err = host.Step(bluehost.SlotOnAir); !errors.Is(err, bluehost.ErrNotLoaded) {
		t.Fatal("old Blue actor still executes on static Program", err)
	}
	// Continue the same author after Take on A, without a Solar feedback callback
	// or sideband observer. Only the real Rust receiver's application ACK is used.
	head, ok := previewSlot.EditableTargetHead("program")
	if !ok || head.SceneID != "scene-c" {
		t.Fatal("active author unavailable", head)
	}
	readRequest := httptest.NewRequest(http.MethodGet, "/api/v1/show/editable-preview/state?target=program&scene_id=scene-c", nil)
	readRequest.Header.Set("X-Authenticated-User", "operator-1")
	readRequest.Header.Set("X-Authenticated-Role", "operator")
	readResponse := httptest.NewRecorder()
	mux.ServeHTTP(readResponse, readRequest)
	var readHead runtime.EditableHead
	if readResponse.Code != http.StatusOK || json.Unmarshal(readResponse.Body.Bytes(), &readHead) != nil || readHead != head || readResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("active read-back failed", readResponse.Body.String())
	}
	patch := map[string]any{"target": "program", "scene_id": "scene-c", "writer_id": head.WriterID, "base_edit_seq": 0, "edit_seq": 1, "patches": []map[string]any{{"path": "__editable.width", "value": 32}}}
	patched := editableAPIRequest(t, mux, http.MethodPut, patch, "operator")
	if patched.Code != http.StatusAccepted {
		t.Fatal("Program hot edit failed", patched.Code, patched.Body.String())
	}
	aNow, _ := r.Read(ctx, "solar/lane-a")
	bNow, _ := r.Read(ctx, "solar/lane-b")
	if aNow.(map[string]any)["defaults"].(map[string]any)["__editable.width"] != float64(32) {
		t.Fatal("Program patch did not reach native A", aNow)
	}
	bBaseline, _ := json.Marshal(bNow)
	patch["target"] = "preview"
	patch["base_edit_seq"] = 1
	patch["edit_seq"] = 2
	if refused := editableAPIRequest(t, mux, http.MethodPut, patch, "operator"); refused.Code != http.StatusConflict {
		t.Fatal("old Preview writer accepted", refused.Body.String())
	}
	patch["target"] = "program"
	patch["writer_id"] = "retired-writer"
	if refused := editableAPIRequest(t, mux, http.MethodPut, patch, "operator"); refused.Code != http.StatusConflict {
		t.Fatal("retired writer accepted", refused.Body.String())
	}
	// Structural replacement stays on current Program A and rotates the writer.
	editable["defaults"].(map[string]any)["__editable.width"] = json.Number("64")
	editable["scene_version"] = "sha256:" + strings.Repeat("0", 64)
	newVersion, err := canonical.Digest(editable)
	if err != nil {
		t.Fatal(err)
	}
	editable["scene_version"] = newVersion
	structure := map[string]any{"target": "program", "scene_id": "scene-c", "scene_version": newVersion, "writer_id": head.WriterID, "base_edit_seq": 1, "edit_seq": 2, "render_mode": "source-only", "lsml_bundle": editable}
	if replaced := editableAPIRequest(t, mux, http.MethodPost, structure, "operator"); replaced.Code != http.StatusOK {
		t.Fatal("Program structural edit failed", replaced.Code, replaced.Body.String())
	}
	newHead, _ := previewSlot.EditableTargetHead("program")
	if newHead.WriterID == head.WriterID || !host.Serving(bluehost.SlotOnAir, "scene-c", newVersion) || !host.Serving(bluehost.SlotPreview, "scene-b", "digest-b") {
		t.Fatal("structural authority drift", newHead)
	}
	if refused := editableAPIRequest(t, mux, http.MethodPost, structure, "operator"); refused.Code != http.StatusConflict {
		t.Fatal("old structural snapshot accepted", refused.Body.String())
	}
	aNow, _ = r.Read(ctx, "solar/lane-a")
	bNow, _ = r.Read(ctx, "solar/lane-b")
	bFinal, _ := json.Marshal(bNow)
	if aNow.(map[string]any)["defaults"].(map[string]any)["__editable.width"] != float64(64) || string(bFinal) != string(bBaseline) {
		t.Fatal("structural Program modified Preview", aNow, bNow)
	}
	commit.TakeCommandID = "take-foreign"
	commit.RuntimeInstanceID = "foreign"
	if err = r.WriteLeaf(ctx, "orion/state", "/scene_control/defaults/lane_commit", commit); err != nil {
		t.Fatal(err)
	}
	if receipt := await("take-foreign", "refused"); receipt.Reason != "LANE_COMMIT_FOREIGN_RUNTIME" {
		t.Fatal(receipt)
	}
	proof, _ := json.Marshal(map[string]any{"first_receipt": firstReceipt, "mixed_receipt": receipt, "documents_identical_at_both_commits": true, "program_delta_isolated": true, "static_program_old_blue_stopped": true, "active_author_readback": true, "program_hot_edit_native": true, "program_structural_edit_native": true, "stale_writers_refused": true, "preview_unchanged": true, "solar_feedback_or_sideband_required": false})
	t.Log(string(proof))
	t.Log("private native receipt, desired/current reconciliation, unchanged documents, retained Program deltas, real Blue-to-editable API/source receipt and foreign-runtime refusal passed")
}
