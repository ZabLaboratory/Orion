package lsdp

// End-to-end / contract test of the meet-cam pipeline, Orion side
// (ADR Blue 009 §5/§7 item 8, issue #262). Probe coverage: the unit tests
// prove each SEAM in isolation — the runtime `assign-slot` op against a
// captureAssigner (exec_assign_slot_test.go), the LSDP mirror against a direct
// EmitSlotAssignment (slot_mirror_test.go), the viewer armer against a stub
// CredsFetcher (viewer_arming_test.go). NONE of them drives the WHOLE chain
// from a single operator fire:
//
//	operator FireExec
//	  → runtime `assign-slot` op (real exec scene)
//	  → curated egress PUT to ZabCam (real ServiceCallClient, mock ZabCam)
//	  → on 2xx: stream-level mirror seam (real Show.emitSlotAssignment)
//	  → LSDP `__cam.slots.<slot>` delta on the real Wire
//	  → viewer arming (real zabcamCredsFetcher, mock ZabCam GET)
//	  → LSDP `__cam.viewer` delta on the real Wire
//	  → both leaves observed on a real LSDP/1.1 WebSocket subscriber.
//
// These tests wire the production graph end-to-end and exercise it through one
// fire, proving the issue's RCs as an INTEGRATION (delta slot, viewer leaf,
// persistence across switch, fail-closed, per-stream isolation, live reassign,
// runtime-not-authored stream_id, token non-leak).
//
// # Mocked boundaries (and why)
//
//   - ZabCam / Meet: an httptest server standing in for ZabGate→ZabCam. It
//     answers BOTH the curated egress upsert (`PUT …/streams/{id}/slots/{ref}`)
//     and the viewer-credentials read (`GET …/cameras/{label}/credentials`).
//     The DURABLE authority is out of process by design (ADR §3.3); the real
//     ZabCam + real Meet signaling are exercised in their own repos. Here we
//     assert the CONTRACT Orion emits/consumes at that frontier.
//   - Solar render: the LSDP subscriber is the kit WS client (lproto). The
//     `meet.peer` re-key / WebRTC join is Solar's job (#28) — out of Orion's
//     process. We assert the WIRE leaf Solar consumes, not the rendered frame
//     (the only valid render proof is a live Twitch VOD, _shared/live-testing).
//   - Restart re-hydration (`GET …/streams/{id}/slots`, ADR §3.4) is NOT
//     covered: no Orion code calls it (grep clean) — flagged to Forge, not a
//     Probe-testable path.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	lproto "github.com/Lumencast/lumencast-go/protocol"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/effects"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// assignRouteE2E is the curated ZabCam slots-assign route the compiler bakes
// under `__route` (verbatim from exec_assign_slot_test.go — same contract).
const assignRouteE2E = `{"service":"zabcam","route_id":"zabcam.slots.assign","method":"PUT",` +
	`"path_template":"/cam/api/v1/streams/{stream_id}/slots/{slot_ref}",` +
	`"params":["stream_id","slot_ref"],"token_paths":["zabcam.slots.assign"]}`

func jsonStr(s string) json.RawMessage { b, _ := json.Marshal(s); return b }

// --- mock ZabCam ----------------------------------------------------------

type camCreds struct{ room, token, ws string }

type recordedPut struct{ escapedPath, body, auth string }

// mockZabCam stands in for ZabGate→ZabCam. It serves the curated slot upsert
// (PUT) and the viewer-credentials read (GET), recording what it received so a
// test can assert the contract at the egress/ingress frontier.
type mockZabCam struct {
	url string

	mu        sync.Mutex
	putStatus int
	puts      []recordedPut
	creds     map[string]camCreds
	getLabels []string
	getAuths  []string
}

func newMockZabCam(t *testing.T) *mockZabCam {
	t.Helper()
	m := &mockZabCam{putStatus: http.StatusNoContent, creds: map[string]camCreds{}}
	srv := httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(srv.Close)
	m.url = srv.URL
	return m
}

func (m *mockZabCam) setCreds(label string, c camCreds) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.creds[label] = c
}

func (m *mockZabCam) setPutStatus(code int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.putStatus = code
}

func (m *mockZabCam) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPut && strings.Contains(r.URL.EscapedPath(), "/slots/"):
		body, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.puts = append(m.puts, recordedPut{r.URL.EscapedPath(), string(body), r.Header.Get("Authorization")})
		status := m.putStatus
		m.mu.Unlock()
		w.WriteHeader(status)
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/credentials"):
		label := strings.TrimSuffix(lastSegmentBefore(r.URL.Path, "/credentials"), "")
		m.mu.Lock()
		m.getLabels = append(m.getLabels, label)
		m.getAuths = append(m.getAuths, r.Header.Get("Authorization"))
		c, ok := m.creds[label]
		m.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"meet_room_id": c.room, "meet_token": c.token, "meet_ws_url": c.ws,
		})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// lastSegmentBefore returns the path segment immediately preceding suffix in p
// (`/cam/.../cameras/<label>/credentials` → `<label>`).
func lastSegmentBefore(p, suffix string) string {
	trimmed := strings.TrimSuffix(p, suffix)
	i := strings.LastIndex(trimmed, "/")
	if i < 0 {
		return trimmed
	}
	return trimmed[i+1:]
}

func (m *mockZabCam) putSnapshot() []recordedPut {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]recordedPut(nil), m.puts...)
}

func (m *mockZabCam) getCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.getLabels)
}

// --- pipeline wiring ------------------------------------------------------

type assignSpec struct{ entry, slot, peer string }

// assignProgram builds an exec program with one `assign-slot` node per spec,
// each on its own entrypoint (so a test can fire reassignments independently),
// the curated route baked under `__route`, slot_ref/peer_label as the author's
// inline literals. No `then`/`error` chain is needed — the mirror seam fires
// inside finishEffect regardless of downstream nodes.
func assignProgram(specs ...assignSpec) *runtime.ExecProgram {
	nodes := map[string]*runtime.ExecNode{}
	entries := map[string]runtime.ExecEntry{}
	for _, s := range specs {
		id := "assign-" + s.entry
		nodes[id] = &runtime.ExecNode{
			ID: id, Op: runtime.OpAssignSlot,
			Config: map[string]json.RawMessage{
				"__route":    json.RawMessage(assignRouteE2E),
				"slot_ref":   jsonStr(s.slot),
				"peer_label": jsonStr(s.peer),
			},
		}
		entries[s.entry] = runtime.ExecEntry{Target: runtime.ExecTarget{Node: id}}
	}
	return &runtime.ExecProgram{BlueprintKey: "bp", Nodes: nodes, Entrypoints: entries}
}

// scopeEchoMint echoes the requested token scope INTO the bearer, so a test can
// prove the tight scope flows end-to-end (`Bearer svc:<scope>`), not just that
// some token was presented.
func scopeEchoMint(paths []string) string { return "svc:" + strings.Join(paths, ",") }

// pipeline is a fully-wired Orion meet-cam stack against a mock ZabCam.
type pipeline struct {
	wire  *Wire
	show  *runtime.Show
	scene *runtime.Scene
}

// buildPipeline wires the production graph: real Wire + viewer arming (real
// zabcamCredsFetcher → mock ZabCam), real Show with the assign-slot exec
// program and a real ServiceCallClient → mock ZabCam, scene-a loaded + active.
func buildPipeline(t *testing.T, logger *slog.Logger, mock *mockZabCam, prog *runtime.ExecProgram) *pipeline {
	t.Helper()
	wire, err := NewWire(logger, nil)
	if err != nil {
		t.Fatalf("NewWire: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	wire.EnableViewerCreds(ctx, NewZabCamCredsFetcher(mock.url, scopeEchoMint, logger), 0)

	show := runtime.NewShow(runtime.NewComputeRegistry(), logger)
	show.SetMirrors(wire)
	t.Cleanup(show.Stop)

	runner := effects.NewRunner(2, 32, logger)
	runner.Start()
	t.Cleanup(runner.Stop)
	show.SetEffects(&runtime.SceneEffects{
		Runner:      runner,
		ServiceCall: effects.NewServiceCallClient(mock.url, scopeEchoMint, nil),
	})

	show.LoadExec("scene-a",
		passthroughGraph("scene-a", "sha256:ver-a", "score.team_a"),
		&compiler.RenderBundle{SceneVersion: "sha256:ver-a"}, prog)
	if err := show.SetActive("scene-a", nil); err != nil {
		t.Fatalf("SetActive scene-a: %v", err)
	}
	scene, err := show.Get("scene-a")
	if err != nil {
		t.Fatalf("Get scene-a: %v", err)
	}
	return &pipeline{wire: wire, show: show, scene: scene}
}

// readLeaves reads server frames until every requested leaf has reached the
// wire (from a Snapshot's State or a Delta's Patches), returning their raw JSON
// values. Fails if any leaf never arrives.
func readLeaves(ctx context.Context, t *testing.T, c *websocket.Conn, leaves ...string) map[string]string {
	t.Helper()
	want := map[string]struct{}{}
	for _, l := range leaves {
		want[l] = struct{}{}
	}
	got := map[string]string{}
	for i := 0; i < 32 && len(got) < len(leaves); i++ {
		switch m := readServerFrame(ctx, t, c).(type) {
		case *lproto.Snapshot:
			for l := range want {
				if v, ok := m.State[l]; ok {
					got[l] = string(v)
				}
			}
		case *lproto.Delta:
			for _, p := range m.Patches {
				if _, ok := want[p.Path]; ok {
					got[p.Path] = string(p.Value)
				}
			}
		}
	}
	if len(got) < len(leaves) {
		t.Fatalf("not all leaves reached the wire: want %v, got %v", leaves, got)
	}
	return got
}

// decodeViewerLeaf decodes the `__cam.viewer` JSON-string-scalar leaf value
// into its rooms.
func decodeViewerLeaf(t *testing.T, raw string) []ViewerRoom {
	t.Helper()
	var js string
	if err := json.Unmarshal([]byte(raw), &js); err != nil {
		t.Fatalf("viewer leaf is not a JSON string scalar: %s (%v)", raw, err)
	}
	var inj viewerInjection
	if err := json.Unmarshal([]byte(js), &inj); err != nil {
		t.Fatalf("viewer payload not decodable: %s (%v)", js, err)
	}
	return inj.Rooms
}

// --- tests ----------------------------------------------------------------

// TestE2E_AssignSlotPipeline is the centerpiece: ONE operator fire drives the
// whole chain. It proves, as an integration (RC4 slot delta + RC3 viewer leaf
// + RC6 runtime stream_id + tight token scope + RC7 token non-leak):
//   - the curated egress PUT reaches ZabCam with the runtime-built path
//     (`stream_id=live`, NEVER authored), the `{peer_label}` body, and a bearer
//     scoped EXACTLY to the route's token_paths;
//   - on 2xx the `__cam.slots.cam-left` delta carries "alice" on the wire;
//   - the viewer armer resolves alice's room via ZabCam (scoped creds read) and
//     carries the receive-only creds on `__cam.viewer`;
//   - the receive-only token never appears in any log line.
func TestE2E_AssignSlotPipeline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const secretTok = "vtok-alice-RECEIVE-ONLY-do-not-log"
	mock := newMockZabCam(t)
	mock.setCreds("alice", camCreds{room: "room-1", token: secretTok, ws: "wss://meet/sig"})

	var logbuf syncBuffer
	logger := slog.New(slog.NewTextHandler(&logbuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	p := buildPipeline(t, logger, mock, assignProgram(assignSpec{"e", "cam-left", "alice"}))

	c := dialLSDP(ctx, t, mountWire(t, p.wire), "viewer", 0)
	defer c.Close(websocket.StatusNormalClosure, "")
	if _, ok := readServerFrame(ctx, t, c).(*lproto.Snapshot); !ok {
		t.Fatal("first frame must be the join snapshot")
	}

	if !p.scene.FireExec("e", "operator:test") {
		t.Fatal("scene inbox full")
	}

	leaves := readLeaves(ctx, t, c, "__cam.slots.cam-left", viewerLeaf)
	if leaves["__cam.slots.cam-left"] != `"alice"` {
		t.Fatalf("slot leaf = %s, want \"alice\"", leaves["__cam.slots.cam-left"])
	}
	rooms := decodeViewerLeaf(t, leaves[viewerLeaf])
	if len(rooms) != 1 || rooms[0].RoomID != "room-1" || rooms[0].SignalingURL != "wss://meet/sig" || rooms[0].JoinToken != secretTok {
		t.Fatalf("viewer rooms = %+v, want one {wss://meet/sig room-1 <secret>}", rooms)
	}

	// Curated egress contract: one PUT, runtime-built path (stream_id=live, NOT
	// authored), body, and a bearer scoped to EXACTLY the route's token_paths.
	puts := mock.putSnapshot()
	if len(puts) != 1 {
		t.Fatalf("ZabCam upserts = %d, want 1", len(puts))
	}
	if puts[0].escapedPath != "/cam/api/v1/streams/live/slots/cam-left" {
		t.Errorf("upsert path = %q, want /cam/api/v1/streams/live/slots/cam-left (stream_id from runtime)", puts[0].escapedPath)
	}
	if puts[0].body != `{"peer_label":"alice"}` {
		t.Errorf("upsert body = %q, want {\"peer_label\":\"alice\"}", puts[0].body)
	}
	if puts[0].auth != "Bearer svc:zabcam.slots.assign" {
		t.Errorf("upsert auth = %q, want bearer scoped to zabcam.slots.assign", puts[0].auth)
	}

	// Viewer creds read: scoped to EXACTLY zabcam.rooms.credentials (defence in
	// depth — tightest scope, never a broad /cam grant).
	mock.mu.Lock()
	getAuths := append([]string(nil), mock.getAuths...)
	mock.mu.Unlock()
	if len(getAuths) == 0 || getAuths[0] != "Bearer svc:zabcam.rooms.credentials" {
		t.Errorf("creds read auth = %v, want bearer scoped to zabcam.rooms.credentials", getAuths)
	}

	// RC7 (non-leak): the receive-only token rode the wire but never a log line.
	if strings.Contains(logbuf.String(), secretTok) {
		t.Fatalf("receive-only viewer token leaked into logs:\n%s", logbuf.String())
	}
}

// TestE2E_FailClosedOnNon2xx proves fail-closed propagates through the WHOLE
// chain: a non-2xx ZabCam upsert emits NEITHER the slot delta NOR the viewer
// leaf, and the viewer armer is never triggered (ZabCam creds never read) — the
// LSDP cache never diverges from the durable authority (ADR R2).
func TestE2E_FailClosedOnNon2xx(t *testing.T) {
	mock := newMockZabCam(t)
	mock.setCreds("alice", camCreds{room: "room-1", token: "vtok", ws: "wss://meet/sig"})
	mock.setPutStatus(http.StatusConflict) // 409 — durable upsert rejected

	p := buildPipeline(t, quietLogger(t), mock, assignProgram(assignSpec{"e", "cam-left", "alice"}))

	if !p.scene.FireExec("e", "operator:test") {
		t.Fatal("scene inbox full")
	}

	// Let the op fire, park on the egress, resume on the 409, and (not) mirror.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(mock.putSnapshot()) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond) // settle window for any (erroneous) mirror

	if puts := mock.putSnapshot(); len(puts) != 1 {
		t.Fatalf("ZabCam upserts = %d, want exactly 1 (the rejected attempt)", len(puts))
	}
	if got := mock.getCount(); got != 0 {
		t.Fatalf("viewer creds read %d times on a rejected upsert, want 0 (armer must stay cold)", got)
	}
	state := p.wire.kitState("scene-a")
	if v, ok := state["__cam.slots.cam-left"]; ok {
		t.Fatalf("slot leaf mirrored on a rejected upsert: %q", v)
	}
	if v, ok := state[viewerLeaf]; ok {
		t.Fatalf("viewer leaf emitted on a rejected upsert: %q", v)
	}
}

// TestE2E_PersistsAcrossSceneSwitch proves the stream-level binding survives a
// real active-scene switch driven through the full chain (RC5): after a
// successful fire on scene-a, the operator switches to scene-b; a LATE joiner
// on scene-b receives BOTH leaves in its keyframe, even though no scene binds
// the `__cam.*` leaves.
func TestE2E_PersistsAcrossSceneSwitch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	mock := newMockZabCam(t)
	mock.setCreds("alice", camCreds{room: "room-1", token: "vtok-1", ws: "wss://meet/sig"})

	p := buildPipeline(t, quietLogger(t), mock, assignProgram(assignSpec{"e", "cam-left", "alice"}))

	if !p.scene.FireExec("e", "operator:test") {
		t.Fatal("scene inbox full")
	}
	// Wait until both leaves have mirrored onto scene-a's kit state.
	waitForState(t, p.wire, p.scene, "__cam.slots.cam-left", `"alice"`)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && p.wire.kitState("scene-a")[viewerLeaf] == "" {
		time.Sleep(5 * time.Millisecond)
	}

	// Operator switches to scene-b (a different scene of the same stream).
	p.show.Load("scene-b", passthroughGraph("scene-b", "sha256:ver-b", "board.row0"),
		&compiler.RenderBundle{SceneVersion: "sha256:ver-b"})
	if err := p.show.SetActive("scene-b", nil); err != nil {
		t.Fatalf("SetActive scene-b: %v", err)
	}

	c := dialLSDP(ctx, t, mountWire(t, p.wire), "viewer", 0)
	defer c.Close(websocket.StatusNormalClosure, "")
	snap, ok := readServerFrame(ctx, t, c).(*lproto.Snapshot)
	if !ok {
		t.Fatal("first frame must be the join snapshot")
	}
	if snap.SceneID != "scene-b" {
		t.Fatalf("keyframe scene_id = %q, want scene-b (active)", snap.SceneID)
	}
	if got := string(snap.State["__cam.slots.cam-left"]); got != `"alice"` {
		t.Fatalf("slot lost across scene switch: keyframe = %q, want \"alice\"", got)
	}
	rooms := decodeViewerStateFromSnapshot(t, snap.State)
	if len(rooms) != 1 || rooms[0].JoinToken != "vtok-1" {
		t.Fatalf("viewer creds lost across scene switch: %+v", rooms)
	}
}

// TestE2E_LiveReassignWithoutSceneSwitch covers the issue's "switch LIVE
// d'assignation sans switch de scène ni re-push": re-firing the SAME slot to a
// DIFFERENT peer re-keys the slot delta AND re-arms the viewer to the new
// peer's room (the old room, now unbound, drops) — no SetActive, no re-push.
func TestE2E_LiveReassignWithoutSceneSwitch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	mock := newMockZabCam(t)
	mock.setCreds("alice", camCreds{room: "room-1", token: "vtok-a", ws: "wss://meet/sig"})
	mock.setCreds("bob", camCreds{room: "room-2", token: "vtok-b", ws: "wss://meet/sig"})

	p := buildPipeline(t, quietLogger(t), mock,
		assignProgram(assignSpec{"e1", "cam-left", "alice"}, assignSpec{"e2", "cam-left", "bob"}))

	c := dialLSDP(ctx, t, mountWire(t, p.wire), "viewer", 0)
	defer c.Close(websocket.StatusNormalClosure, "")
	if _, ok := readServerFrame(ctx, t, c).(*lproto.Snapshot); !ok {
		t.Fatal("first frame must be the join snapshot")
	}

	// First assignment: alice.
	if !p.scene.FireExec("e1", "operator:test") {
		t.Fatal("inbox full")
	}
	first := readLeaves(ctx, t, c, "__cam.slots.cam-left", viewerLeaf)
	if first["__cam.slots.cam-left"] != `"alice"` {
		t.Fatalf("first slot = %s, want \"alice\"", first["__cam.slots.cam-left"])
	}

	// Reassign the SAME slot to bob — no scene switch, no re-push.
	if !p.scene.FireExec("e2", "operator:test") {
		t.Fatal("inbox full")
	}
	// Wait until the slot has re-keyed to bob on the kit state, then read the
	// fresh viewer leaf carrying bob's room (room-2) and only that.
	waitForState(t, p.wire, p.scene, "__cam.slots.cam-left", `"bob"`)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rooms := decodeViewerStateFromSnapshot(t, rawState(p.wire, "scene-a"))
		if len(rooms) == 1 && rooms[0].RoomID == "room-2" && rooms[0].JoinToken == "vtok-b" {
			break
		}
		if time.Now().After(deadline.Add(-5 * time.Millisecond)) {
			t.Fatalf("viewer never re-armed to bob's room: %+v", rooms)
		}
		time.Sleep(5 * time.Millisecond)
	}

	if got := len(mock.putSnapshot()); got != 2 {
		t.Fatalf("ZabCam upserts = %d, want 2 (one per assignment)", got)
	}
}

// rawState reads a kit scene's `__cam.viewer` leaf as a json.RawMessage map for
// decodeViewerStateFromSnapshot.
func rawState(w *Wire, sceneID string) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for k, v := range w.kitState(sceneID) {
		out[k] = json.RawMessage(v)
	}
	return out
}

// TestE2E_IsolatedPerStream proves per-stream isolation through the full chain:
// two independent pipelines (one Wire/Show each — the per-stream boundary); a
// fire on stream A's wire leaks NEITHER the slot NOR the viewer leaf onto stream
// B's wire.
func TestE2E_IsolatedPerStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	mockA := newMockZabCam(t)
	mockA.setCreds("alice", camCreds{room: "room-1", token: "vtok-1", ws: "wss://meet/sig"})
	mockB := newMockZabCam(t)

	pa := buildPipeline(t, quietLogger(t), mockA, assignProgram(assignSpec{"e", "cam-left", "alice"}))
	pb := buildPipeline(t, quietLogger(t), mockB, assignProgram(assignSpec{"e", "cam-left", "alice"}))

	if !pa.scene.FireExec("e", "operator:test") {
		t.Fatal("inbox full")
	}
	waitForState(t, pa.wire, pa.scene, "__cam.slots.cam-left", `"alice"`)

	// Stream B's late joiner must see NEITHER leaf.
	c := dialLSDP(ctx, t, mountWire(t, pb.wire), "viewer", 0)
	defer c.Close(websocket.StatusNormalClosure, "")
	snap, ok := readServerFrame(ctx, t, c).(*lproto.Snapshot)
	if !ok {
		t.Fatal("first frame must be the join snapshot")
	}
	if _, leaked := snap.State["__cam.slots.cam-left"]; leaked {
		t.Fatalf("stream B leaked stream A's slot binding: %v", snap.State)
	}
	if _, leaked := snap.State[viewerLeaf]; leaked {
		t.Fatalf("stream B leaked stream A's viewer creds: %v", snap.State)
	}
	if got := mockB.getCount(); got != 0 {
		t.Fatalf("stream B's ZabCam saw %d creds reads, want 0 (no cross-stream arming)", got)
	}
}

// TestE2E_AntiInjectionTemplateThroughWire proves the egress path is built from
// the curated template with the slot_ref escaped (RC6) through the FULL chain: a
// slot_ref carrying path traversal cannot escape its segment — the upsert URL
// stays one segment under the curated template, and the WIRE leaf is keyed by
// the raw (un-mangled) slot_ref the author wrote.
func TestE2E_AntiInjectionTemplateThroughWire(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	mock := newMockZabCam(t)
	const evil = "../../rooms/evil/delete"
	mock.setCreds("alice", camCreds{room: "room-1", token: "vtok-1", ws: "wss://meet/sig"})

	p := buildPipeline(t, quietLogger(t), mock, assignProgram(assignSpec{"e", evil, "alice"}))

	c := dialLSDP(ctx, t, mountWire(t, p.wire), "viewer", 0)
	defer c.Close(websocket.StatusNormalClosure, "")
	if _, ok := readServerFrame(ctx, t, c).(*lproto.Snapshot); !ok {
		t.Fatal("first frame must be the join snapshot")
	}

	if !p.scene.FireExec("e", "operator:test") {
		t.Fatal("inbox full")
	}
	leaves := readLeaves(ctx, t, c, "__cam.slots."+evil, viewerLeaf)
	if leaves["__cam.slots."+evil] != `"alice"` {
		t.Fatalf("slot leaf = %s, want \"alice\"", leaves["__cam.slots."+evil])
	}

	puts := mock.putSnapshot()
	if len(puts) != 1 {
		t.Fatalf("upserts = %d, want 1", len(puts))
	}
	const want = "/cam/api/v1/streams/live/slots/..%2F..%2Frooms%2Fevil%2Fdelete"
	if puts[0].escapedPath != want {
		t.Fatalf("upsert path = %q, want %q (slot_ref must stay one escaped segment)", puts[0].escapedPath, want)
	}
}
