package api

import (
	"context"
	"encoding/json"
	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/bluewire"
	"github.com/ZabLaboratory/Orion/internal/canonical"
	"github.com/ZabLaboratory/Orion/internal/config"
	"github.com/ZabLaboratory/Orion/internal/lsdpreception"
	"github.com/ZabLaboratory/Orion/internal/protocol"
	"github.com/ZabLaboratory/Orion/internal/runtime"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This test owns a disposable Rust receiver, never the common application.
func TestProgramReleaseNative(t *testing.T) {
	address := os.Getenv("ORION_TEST_PRIVATE_LANE_ADDRESS")
	if address == "" {
		t.Skip("requires private native receiver")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	reception, err := lsdpreception.New(address, "orion/state")
	if err != nil {
		t.Fatal(err)
	}
	p := lsdpreception.NewProducer(ctx, reception)
	defer p.Close()
	p.Replace("solar/lane-a", nil)
	p.Replace("solar/lane-b", nil)
	if err = p.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	hub := lsdpreception.NewHub(p, slog.Default())
	hub.EnablePhysicalLanes()
	program, preview := hub.Lane("program"), hub.Lane("preview")
	host := bluehost.NewHost()
	bridges := bluewire.NewRegistry()
	defer bridges.StopAll()
	authors := runtime.NewPreviewSlot(ctx, runtime.NewComputeRegistry(), preview, slog.Default())
	authors.SetEditableProgramWire(program)
	defer authors.Close()
	intent := &SceneIntentDeps{Host: host, Bridges: bridges, CommitLaneRoles: NativeLaneCommitter(hub, host, bridges, authors), CheckProgramRelease: NativeProgramReleaseChecker(hub, authors), ReleaseProgram: NativeProgramReleaser(hub, host, bridges, authors), InspectProgram: NativeProgramInspector(hub, host, authors)}
	mux := http.NewServeMux()
	RegisterPublic(mux, PublicDeps{Logger: slog.Default(), Config: config.Config{Profile: config.ProfileEmbeddedLocal}, Preview: authors, SceneIntent: intent, NativeLSDPFlush: p.Flush})
	open := func(id string) runtime.EditableHead {
		t.Helper()
		doc := map[string]any{"lsml": "1.2", "scene_id": id, "scene_version": "sha256:" + strings.Repeat("0", 64), "layout": map[string]any{"kind": "shape", "bind": map[string]any{"width": "__editable.width"}}, "defaults": map[string]any{"__editable.width": json.Number("10")}}
		version, err := canonical.Digest(doc)
		if err != nil {
			t.Fatal(err)
		}
		doc["scene_version"] = version
		got := editableAPIRequest(t, mux, http.MethodPost, map[string]any{"scene_id": id, "scene_version": version, "render_mode": "source-only", "edit_seq": 0, "lsml_bundle": doc}, "operator")
		if got.Code != http.StatusOK {
			t.Fatal(got.Code, got.Body.String())
		}
		head, ok := authors.EditableTargetHead("preview")
		if !ok {
			t.Fatal("Preview author missing")
		}
		return head
	}
	read := func(target string) any {
		t.Helper()
		state, err := reception.Read(ctx, target)
		if err != nil {
			t.Fatal(err)
		}
		return state
	}
	bytes := func(v any) string { raw, _ := json.Marshal(v); return string(raw) }
	first := open("release-live")
	d := NewSceneControlDocument()
	if err = reception.WriteLeaf(ctx, "orion/state", "/scene_control", d); err != nil {
		t.Fatal(err)
	}
	c := &SceneControl{deps: *intent, reception: reception, path: filepath.Join(t.TempDir(), "selection.lsml"), desired: d.Defaults.Desired, current: map[string]string{}}
	runCtx, stopRun := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); _ = c.Run(runCtx) }()
	defer func() { stopRun(); <-done }()
	commit := lsdpreception.LaneCommit{Schema: "orion.lane-commit.v1", RuntimeInstanceID: "release-native-runtime", PrepareCommandID: "first-prepare", TakeCommandID: "first-take", IntentID: "first-intent", FrameID: 12, PtsNs: 1234, ServerSeq: 4, PreviousRoleMap: lsdpreception.RoleMap{OnAir: "A", Preview: "B"}, RoleMap: lsdpreception.RoleMap{OnAir: "B", Preview: "A"}, PreviousRevisions: lsdpreception.LaneRevisions{Program: 0, Preview: 1, RoleMap: 0}, Revisions: lsdpreception.LaneRevisions{Program: 1, Preview: 1, RoleMap: 1}, SceneID: first.SceneID, SceneVersion: first.SceneVersion}
	if err = reception.WriteLeaf(ctx, "orion/state", "/scene_control/defaults/lane_commit", commit); err != nil {
		t.Fatal(err)
	}
	control := func() SceneControlDocument {
		t.Helper()
		raw, _ := json.Marshal(read("orion/state").(map[string]any)["scene_control"])
		var got SceneControlDocument
		if err = json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	wait := func(check func(SceneControlDocument) bool) {
		t.Helper()
		for ctx.Err() == nil {
			if check(control()) {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("native control receipt timed out")
	}
	wait(func(d SceneControlDocument) bool {
		r, _ := d.Defaults.Observed["lane_commit"].(map[string]any)
		return r["status"] == "committed"
	})
	if read("solar/lane-a") != nil || read("solar/lane-b").(map[string]any)["scene_id"] != first.SceneID {
		t.Fatal("first start duplicated/reloaded Preview")
	}
	live, ok := authors.EditableTargetHead("program")
	if !ok || live.WriterID != first.WriterID {
		t.Fatal("start lost original writer")
	}
	oldMirror := authors.EditableTargetMirror("program")
	second := open("release-preview")
	previewBefore := bytes(read("solar/lane-a"))
	observedBefore := control().Defaults.Observed["preview"]
	inspect := lsdpreception.ProgramInspect{Schema: "orion.program-inspect.v1", CommandID: "inspect-1", RuntimeInstanceID: commit.RuntimeInstanceID, TakeCommandID: commit.TakeCommandID, ExpectedRoleMap: commit.RoleMap, ExpectedRevisions: commit.Revisions}
	inspect.ExpectedRevisions.Preview += 10 // independent Preview preparations are allowed
	if err = reception.WriteLeaf(ctx, "orion/state", "/scene_control/defaults/inspect_program", inspect); err != nil {
		t.Fatal(err)
	}
	wait(func(d SceneControlDocument) bool {
		got, _ := d.Defaults.Observed["inspect_program"].(map[string]any)
		return got["command_id"] == inspect.CommandID && got["status"] == "active"
	})
	raw, _ := json.Marshal(control().Defaults.Observed["inspect_program"])
	var identity lsdpreception.ProgramIdentityReceipt
	if err = json.Unmarshal(raw, &identity); err != nil || identity.WriterID != live.WriterID || identity.EditSeq == nil || *identity.EditSeq != live.EditSeq {
		t.Fatal("native inspection lost author", identity, err)
	}
	seq := *identity.EditSeq
	r := lsdpreception.ProgramRelease{Schema: "orion.program-release.v1", CommandID: "release-1", StopCommandID: "confirmed-stop-1", OutputState: "stopped", RuntimeInstanceID: commit.RuntimeInstanceID, TakeCommandID: commit.TakeCommandID, ExpectedRoleMap: commit.RoleMap, ExpectedRevisions: commit.Revisions, SceneID: live.SceneID, SceneVersion: live.SceneVersion, ArtifactSet: live.SceneVersion, WriterID: live.WriterID, EditSeq: &seq}
	patched := editableAPIRequest(t, mux, http.MethodPut, map[string]any{"target": "program", "scene_id": live.SceneID, "writer_id": live.WriterID, "base_edit_seq": seq, "edit_seq": seq + 1, "patches": []map[string]any{{"path": "__editable.width", "value": 12}}}, "operator")
	if patched.Code != http.StatusAccepted {
		t.Fatal(patched.Code, patched.Body.String())
	}
	if stale := intent.ReleaseProgram(ctx, r); stale.Reason != "PROGRAM_RELEASE_WRITER_MISMATCH" {
		t.Fatal("inspection failed to fence intervening edits", stale)
	}
	seq++
	previewSnapshot, _ := authors.EditableTargetSnapshot("preview")
	programSnapshot, _ := authors.EditableTargetSnapshot("program")
	for _, tc := range []struct {
		name, reason string
		change       func(*lsdpreception.ProgramRelease)
	}{
		{"runtime", "PROGRAM_RELEASE_FOREIGN_RUNTIME", func(r *lsdpreception.ProgramRelease) { r.RuntimeInstanceID = "foreign" }},
		{"epoch", "PROGRAM_RELEASE_STALE_EPOCH", func(r *lsdpreception.ProgramRelease) { r.ExpectedRevisions.RoleMap++ }},
		{"identity", "PROGRAM_RELEASE_IDENTITY_MISMATCH", func(r *lsdpreception.ProgramRelease) { r.SceneID = "retired" }},
		{"writer", "PROGRAM_RELEASE_WRITER_MISMATCH", func(r *lsdpreception.ProgramRelease) { r.WriterID = "retired-writer" }},
	} {
		bad := r
		bad.CommandID = "bad-" + tc.name
		tc.change(&bad)
		got := intent.ReleaseProgram(ctx, bad)
		if got.Reason != tc.reason || bytes(read("solar/lane-a")) != previewBefore || read("solar/lane-b") == nil {
			t.Fatal(tc.name, got)
		}
	}
	// A pending Program admission is cancelled and joined; its delayed result
	// cannot reoccupy the slot after release. Preview work is not cancelled.
	jobCtx, stopJob := context.WithCancel(ctx)
	defer stopJob()
	previewJob, stopPreviewJob := context.WithCancel(ctx)
	defer stopPreviewJob()
	c.opMu.Lock()
	c.mu.Lock()
	c.inFlight = map[string]context.CancelFunc{"program": stopJob, "preview": stopPreviewJob}
	c.mu.Unlock()
	joined := make(chan struct{})
	go func() { <-jobCtx.Done(); c.opMu.Unlock(); close(joined) }()
	if err = reception.WriteLeaf(ctx, "orion/state", "/scene_control/defaults/release_program", r); err != nil {
		t.Fatal(err)
	}
	wait(func(d SceneControlDocument) bool {
		got, _ := d.Defaults.Observed["release_program"].(map[string]any)
		return got["command_id"] == r.CommandID && got["status"] == "released"
	})
	<-joined
	if previewJob.Err() != nil || bytes(read("solar/lane-a")) != previewBefore || read("solar/lane-b") != nil {
		t.Fatal("release touched Preview or retained Program")
	}
	if _, ok = authors.EditableTargetHead("program"); ok || host.Digest(bluehost.SlotOnAir) != "" {
		t.Fatal("retired owner remains exposed")
	}
	if got, _ := authors.EditableTargetHead("preview"); got != second {
		t.Fatal("Preview writer changed")
	}
	if now, _ := authors.EditableTargetSnapshot("preview"); now != previewSnapshot {
		t.Fatal("release invalidated Preview compilation")
	}
	if now, _ := authors.EditableTargetSnapshot("program"); now == programSnapshot {
		t.Fatal("release retained Program compilation authority")
	}
	final := control()
	if _, exists := final.Defaults.Desired["program"]; exists || bytes(final.Defaults.Observed["preview"]) != bytes(observedBefore) {
		t.Fatal("release did not reconcile only Program")
	}
	saved, err := os.ReadFile(c.path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted SceneControlDocument
	if json.Unmarshal(saved, &persisted) != nil || len(persisted.Defaults.Observed) != 0 || persisted.Defaults.ReleaseProgram != nil {
		t.Fatal("release runtime command was persisted")
	}
	oldMirror.Forward(&protocol.Delta{Patches: []protocol.Patch{{Path: "__editable.width", Value: json.RawMessage(`99`)}}})
	if err = p.Flush(ctx); err != nil || read("solar/lane-b") != nil {
		t.Fatal("late publication recreated released Program", err)
	}
	if got := intent.ReleaseProgram(ctx, r); got.Status != "released" || !got.Replayed {
		t.Fatal("release replay changed authority", got)
	}
	// The next real Take promotes the still-existing Preview without reloading.
	next := commit
	next.PrepareCommandID = "second-prepare"
	next.TakeCommandID = "second-take"
	next.IntentID = "second-intent"
	next.FrameID++
	next.PtsNs++
	next.ServerSeq++
	next.PreviousRoleMap = commit.RoleMap
	next.RoleMap = commit.PreviousRoleMap
	next.PreviousRevisions = commit.Revisions
	next.Revisions.Program++
	next.Revisions.RoleMap++
	next.SceneID = second.SceneID
	next.SceneVersion = second.SceneVersion
	if got := intent.CommitLaneRoles(ctx, next); got.Status != "committed" {
		t.Fatal(got)
	}
	if epoch, allowed := host.ProgramAdmissionToken(); !allowed || !host.ProgramAdmissionValid(epoch) {
		t.Fatal("editable physical start did not reopen Program admissions")
	}
	if bytes(read("solar/lane-a")) != previewBefore || read("solar/lane-b") != nil {
		t.Fatal("next start reloaded or duplicated document")
	}
	if got, ok := authors.EditableTargetHead("program"); !ok || got.WriterID != second.WriterID {
		t.Fatal("next start did not transfer writer")
	}
	if got := intent.ReleaseProgram(ctx, r); got.Status != "released" || !got.Replayed || read("solar/lane-a") == nil {
		t.Fatal("old release replay retired new Program", got)
	}
	t.Logf("REAL_NATIVE_PROGRAM_RELEASE_PROOF first-start-empty=true retired=solar/lane-b preview_unchanged=true late_publication_refused=true next-start-no-reload=true receipt=%s", bytes(final.Defaults.Observed["release_program"]))
}

func TestProgramReleaseBlueNative(t *testing.T) {
	address := os.Getenv("ORION_TEST_PRIVATE_LANE_ADDRESS")
	if address == "" {
		t.Skip("requires private native receiver")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	reception, err := lsdpreception.New(address, "orion/state")
	if err != nil {
		t.Fatal(err)
	}
	p := lsdpreception.NewProducer(ctx, reception)
	defer p.Close()
	p.Replace("solar/lane-a", nil)
	p.Replace("solar/lane-b", nil)
	hub := lsdpreception.NewHub(p, slog.Default())
	hub.EnablePhysicalLanes()
	program, preview := hub.Lane("program"), hub.Lane("preview")
	preview.MirrorForLSML("blue-live", "blue-artifacts", []byte(`{"lsml":"1.2","scene_id":"blue-live","scene_version":"blue-source","layout":{"type":"frame","children":[]},"defaults":{}}`))
	preview.SetActive("blue-live")
	if err = p.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	host := bluehost.NewHost()
	fixture := minimalProgram(t)
	if err = host.Prepare(bluehost.SlotPreview, "blue-actor", "blue-live", "blue-digest", fixture, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = host.Step(bluehost.SlotPreview); err != nil {
		t.Fatal(err)
	}
	bridges := bluewire.NewRegistry()
	defer bridges.StopAll()
	authors := runtime.NewPreviewSlot(ctx, runtime.NewComputeRegistry(), preview, slog.Default())
	authors.SetEditableProgramWire(program)
	defer authors.Close()
	commit := lsdpreception.LaneCommit{Schema: "orion.lane-commit.v1", RuntimeInstanceID: "blue-native-runtime", PrepareCommandID: "blue-prepare", TakeCommandID: "blue-take", IntentID: "blue-intent", FrameID: 12, PtsNs: 1234, ServerSeq: 4, PreviousRoleMap: lsdpreception.RoleMap{OnAir: "A", Preview: "B"}, RoleMap: lsdpreception.RoleMap{OnAir: "B", Preview: "A"}, PreviousRevisions: lsdpreception.LaneRevisions{Preview: 1}, Revisions: lsdpreception.LaneRevisions{Program: 1, Preview: 1, RoleMap: 1}, SceneID: "blue-live", SceneVersion: "blue-source"}
	if got := NativeLaneCommitter(hub, host, bridges, authors)(ctx, commit); got.Status != "committed" {
		t.Fatal(got)
	}
	if _, err = host.Step(bluehost.SlotOnAir); err != nil {
		t.Fatal("promoted Blue did not execute", err)
	}
	r := lsdpreception.ProgramRelease{Schema: "orion.program-release.v1", CommandID: "blue-release", StopCommandID: "blue-stop", OutputState: "stopped", RuntimeInstanceID: commit.RuntimeInstanceID, TakeCommandID: commit.TakeCommandID, ExpectedRoleMap: commit.RoleMap, ExpectedRevisions: commit.Revisions, SceneID: commit.SceneID, SceneVersion: commit.SceneVersion, ArtifactSet: "blue-artifacts"}
	if got := NativeProgramReleaser(hub, host, bridges, authors)(ctx, r); got.Status != "released" {
		t.Fatal(got)
	}
	if _, err = host.Step(bluehost.SlotOnAir); err == nil {
		t.Fatal("released Blue still steps")
	}
	state, err := reception.Read(ctx, "solar/lane-b")
	if err != nil || state != nil {
		t.Fatal("Blue document retained", state, err)
	}
	t.Log("REAL_NATIVE_BLUE_RELEASE_PROOF SDK instance stopped, physical Program null, next steps refused")
}
