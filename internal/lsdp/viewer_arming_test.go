package lsdp

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	lproto "github.com/Lumencast/lumencast-go/protocol"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/runtime"
)

// stubFetcher is an in-memory CredsFetcher. It records the peer_labels it was
// asked to resolve and returns canned ViewerRooms, so a test can assert the
// arming logic without any network.
type stubFetcher struct {
	mu      sync.Mutex
	byLabel map[string]ViewerRoom
	delay   map[string]time.Duration
	asked   []string
}

func newStubFetcher() *stubFetcher {
	return &stubFetcher{byLabel: map[string]ViewerRoom{}, delay: map[string]time.Duration{}}
}

func (s *stubFetcher) set(label string, vr ViewerRoom) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byLabel[label] = vr
}

// setDelay makes FetchViewerCreds block for d before resolving label — an
// artificial per-peer network latency used to prove the arming pass resolves
// every peer concurrently within the total window rather than serially.
func (s *stubFetcher) setDelay(label string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delay[label] = d
}

func (s *stubFetcher) FetchViewerCreds(ctx context.Context, label string) (ViewerRoom, bool) {
	// Read canned state under the lock, then release it BEFORE sleeping so
	// concurrent fetches are not serialised on the stub's own mutex (that would
	// mask whether rearm parallelises).
	s.mu.Lock()
	s.asked = append(s.asked, label)
	vr, ok := s.byLabel[label]
	d := s.delay[label]
	s.mu.Unlock()

	if d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return ViewerRoom{}, false
		}
	}
	return vr, ok
}

// armedWire builds a Wire with viewer arming enabled (refresh disabled — tests
// drive rearm via the slot seam or directly), a live show, and one active
// scene. Returns the wire, the stub fetcher and the show.
func armedWire(t *testing.T) (*Wire, *stubFetcher, *runtime.Show) {
	t.Helper()
	logger := quietLogger(t)
	wire, err := NewWire(logger, nil)
	if err != nil {
		t.Fatalf("NewWire: %v", err)
	}
	fetch := newStubFetcher()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	wire.EnableViewerCreds(ctx, fetch, 0) // ticker off; arm on peer-set change

	show := runtime.NewShow(runtime.NewComputeRegistry(), logger)
	show.SetMirrors(wire)
	t.Cleanup(show.Stop)
	show.Load("scene-a", passthroughGraph("scene-a", "sha256:ver-a", "score.team_a"),
		&compiler.RenderBundle{SceneVersion: "sha256:ver-a"})
	if err := show.SetActive("scene-a", nil); err != nil {
		t.Fatalf("SetActive A: %v", err)
	}
	return wire, fetch, show
}

// readUntilViewer reads server frames until one carries __cam.viewer, decodes
// it, and returns the rooms. Fails if the leaf never reaches the wire.
func readUntilViewer(ctx context.Context, t *testing.T, c *websocket.Conn) []ViewerRoom {
	t.Helper()
	raw := readUntilSlot(ctx, t, c, viewerLeaf)
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

// TestViewer_EmitsReceiveOnlyCredsOnSlotAssignment (RC3 transport): assigning a
// stream-level slot to a `peer_label` arms that camera's room — Orion resolves
// its receive-only viewer credentials and carries them on `__cam.viewer` as a
// JSON-string scalar in the exact shape Solar #28 consumes.
func TestViewer_EmitsReceiveOnlyCredsOnSlotAssignment(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wire, fetch, _ := armedWire(t)
	fetch.set("alice", ViewerRoom{SignalingURL: "wss://meet/sig", RoomID: "room-1", JoinToken: "vtok-1"})

	c := dialLSDP(ctx, t, mountWire(t, wire), "viewer", 0)
	defer c.Close(websocket.StatusNormalClosure, "")
	if _, ok := readServerFrame(ctx, t, c).(*lproto.Snapshot); !ok {
		t.Fatal("first frame must be the join snapshot")
	}

	wire.EmitSlotAssignment("cam-left", "alice")

	rooms := readUntilViewer(ctx, t, c)
	if len(rooms) != 1 {
		t.Fatalf("rooms = %d, want 1: %+v", len(rooms), rooms)
	}
	got := rooms[0]
	if got.RoomID != "room-1" || got.SignalingURL != "wss://meet/sig" || got.JoinToken != "vtok-1" {
		t.Fatalf("viewer room = %+v, want {wss://meet/sig room-1 vtok-1}", got)
	}
}

// TestViewer_DedupesRoomsAcrossPeers: two armed peer_labels that resolve to the
// SAME live room appear once on the wire (deduplicated by room id) — Solar joins
// each room once.
func TestViewer_DedupesRoomsAcrossPeers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wire, fetch, _ := armedWire(t)
	shared := ViewerRoom{SignalingURL: "wss://meet/sig", RoomID: "room-1", JoinToken: "vtok-1"}
	fetch.set("alice", shared)
	fetch.set("bob", shared)

	c := dialLSDP(ctx, t, mountWire(t, wire), "viewer", 0)
	defer c.Close(websocket.StatusNormalClosure, "")
	if _, ok := readServerFrame(ctx, t, c).(*lproto.Snapshot); !ok {
		t.Fatal("first frame must be the join snapshot")
	}

	wire.EmitSlotAssignment("cam-left", "alice")
	wire.EmitSlotAssignment("cam-right", "bob")

	// Poll the kit state until both peers are armed and deduped to one room.
	deadline := time.Now().Add(2 * time.Second)
	for {
		rooms := decodeViewerState(t, wire, "scene-a")
		if len(rooms) == 1 && rooms[0].RoomID == "room-1" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("rooms never deduped to one: %+v", rooms)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestViewer_RotationReEmitsFreshToken (TTL/rotation): when a room's short-lived
// viewer token rotates, a fresh re-arm replaces it on the wire — proving the
// short-lived refresh path carries a new credential without a scene switch.
func TestViewer_RotationReEmitsFreshToken(t *testing.T) {
	wire, fetch, _ := armedWire(t)
	fetch.set("alice", ViewerRoom{SignalingURL: "wss://meet/sig", RoomID: "room-1", JoinToken: "vtok-1"})

	// First arm (driven directly — deterministic, no ticker flake).
	wire.viewer.setPeers([]string{"alice"})
	waitForViewerToken(t, wire, "scene-a", "room-1", "vtok-1")

	// The room's token rotates (ZabCam re-mints a fresh short-lived viewer
	// credential); a refresh re-fetches and re-emits it.
	fetch.set("alice", ViewerRoom{SignalingURL: "wss://meet/sig", RoomID: "room-1", JoinToken: "vtok-2"})
	wire.viewer.rearm()
	waitForViewerToken(t, wire, "scene-a", "room-1", "vtok-2")
}

// TestViewer_ArmsAllPeersConcurrentlyUnderLatency proves the arming pass
// resolves EVERY peer within the single 5s wall-clock window even when each
// per-peer fetch is slow. Run sequentially the cumulative latency
// (3 + 3.5 + 4 = 10.5s) blows past viewerFetchTimeout and starves the peers
// late in the batch — they silently drop and their rooms never reach the wire,
// leaving the on-air `meet-peer` slots stuck on the placeholder. Run
// concurrently the pass costs only max(delays) < 5s and all three rooms arm.
func TestViewer_ArmsAllPeersConcurrentlyUnderLatency(t *testing.T) {
	wire, fetch, _ := armedWire(t)

	peers := []struct {
		label, room, tok string
		delay            time.Duration
	}{
		{"alice", "room-1", "vtok-1", 3 * time.Second},
		{"bob", "room-2", "vtok-2", 3500 * time.Millisecond},
		{"carol", "room-3", "vtok-3", 4 * time.Second},
	}
	labels := make([]string, 0, len(peers))
	for _, p := range peers {
		fetch.set(p.label, ViewerRoom{SignalingURL: "wss://meet/sig", RoomID: p.room, JoinToken: p.tok})
		fetch.setDelay(p.label, p.delay)
		labels = append(labels, p.label)
	}

	start := time.Now()
	wire.viewer.setPeers(labels) // drives one rearm on the armer goroutine

	// All three rooms must arm. Poll with slack past the total window; a
	// sequential rearm could never satisfy this (its later peers time out).
	deadline := time.Now().Add(viewerFetchTimeout + 3*time.Second)
	for {
		rooms := decodeViewerState(t, wire, "scene-a")
		if len(rooms) == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d/3 rooms armed under latency (concurrent pass regressed to sequential?): %+v",
				len(rooms), rooms)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The pass completed in ~max(delays), not the sequential sum — proof the
	// fetches actually overlapped.
	if elapsed := time.Since(start); elapsed > viewerFetchTimeout {
		t.Fatalf("arming took %v — expected concurrent resolution under %v", elapsed, viewerFetchTimeout)
	}
}

// TestViewer_PersistsAcrossSceneSwitch: viewer creds are stream-level — a
// late joiner on the NEW active scene still gets them in its keyframe.
func TestViewer_PersistsAcrossSceneSwitch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wire, fetch, show := armedWire(t)
	fetch.set("alice", ViewerRoom{SignalingURL: "wss://meet/sig", RoomID: "room-1", JoinToken: "vtok-1"})

	wire.EmitSlotAssignment("cam-left", "alice")
	waitForViewerToken(t, wire, "scene-a", "room-1", "vtok-1")

	// Operator switches to scene-b (a different scene of the same stream).
	show.Load("scene-b", passthroughGraph("scene-b", "sha256:ver-b", "board.row0"),
		&compiler.RenderBundle{SceneVersion: "sha256:ver-b"})
	if err := show.SetActive("scene-b", nil); err != nil {
		t.Fatalf("SetActive B: %v", err)
	}

	c := dialLSDP(ctx, t, mountWire(t, wire), "viewer", 0)
	defer c.Close(websocket.StatusNormalClosure, "")
	snap, ok := readServerFrame(ctx, t, c).(*lproto.Snapshot)
	if !ok {
		t.Fatal("first frame must be the join snapshot")
	}
	if snap.SceneID != "scene-b" {
		t.Fatalf("keyframe scene_id = %q, want scene-b", snap.SceneID)
	}
	rooms := decodeViewerStateFromSnapshot(t, snap.State)
	if len(rooms) != 1 || rooms[0].JoinToken != "vtok-1" {
		t.Fatalf("viewer creds lost across switch: %+v", rooms)
	}
}

// TestViewer_IsolatedPerStream: one stream's viewer creds never leak onto
// another stream's wire — each Wire holds its own armer/derived cache.
func TestViewer_IsolatedPerStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wireA, fetchA, _ := armedWire(t)
	wireB, _, _ := armedWire(t)
	fetchA.set("alice", ViewerRoom{SignalingURL: "wss://meet/sig", RoomID: "room-1", JoinToken: "vtok-1"})

	wireA.EmitSlotAssignment("cam-left", "alice")
	waitForViewerToken(t, wireA, "scene-a", "room-1", "vtok-1")

	// Stream B's late joiner must NOT see A's viewer creds.
	c := dialLSDP(ctx, t, mountWire(t, wireB), "viewer", 0)
	defer c.Close(websocket.StatusNormalClosure, "")
	snap, ok := readServerFrame(ctx, t, c).(*lproto.Snapshot)
	if !ok {
		t.Fatal("first frame must be the join snapshot")
	}
	if _, leaked := snap.State[viewerLeaf]; leaked {
		t.Fatalf("stream B leaked stream A's viewer creds: %v", snap.State)
	}
}

// TestViewer_NeverLogsToken (R1 non-leak): the receive-only viewer token never
// appears in any log line — the armer logs counts only, the fetcher logs status
// only. Drives a full arm with a recognisable token and scans the captured log.
func TestViewer_NeverLogsToken(t *testing.T) {
	var buf syncBuffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	wire, err := NewWire(logger, nil)
	if err != nil {
		t.Fatalf("NewWire: %v", err)
	}
	fetch := newStubFetcher()
	const secret = "SECRET-VIEWER-TOKEN-do-not-log"
	fetch.set("alice", ViewerRoom{SignalingURL: "wss://meet/sig", RoomID: "room-1", JoinToken: secret})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wire.EnableViewerCreds(ctx, fetch, 0)

	show := runtime.NewShow(runtime.NewComputeRegistry(), logger)
	show.SetMirrors(wire)
	t.Cleanup(show.Stop)
	show.Load("scene-a", passthroughGraph("scene-a", "sha256:ver-a", "score.team_a"),
		&compiler.RenderBundle{SceneVersion: "sha256:ver-a"})
	if err := show.SetActive("scene-a", nil); err != nil {
		t.Fatalf("SetActive: %v", err)
	}

	wire.EmitSlotAssignment("cam-left", "alice")
	waitForViewerToken(t, wire, "scene-a", "room-1", secret)

	if got := buf.String(); strings.Contains(got, secret) {
		t.Fatalf("viewer token leaked into logs:\n%s", got)
	}
}

// syncBuffer is a goroutine-safe bytes buffer for capturing slog output (the
// armer logs from its own goroutine).
type syncBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// --- helpers --------------------------------------------------------------

// decodeViewerState reads the kit scene's current __cam.viewer leaf and decodes
// its rooms ([] when absent).
func decodeViewerState(t *testing.T, w *Wire, sceneID string) []ViewerRoom {
	t.Helper()
	raw := w.kitState(sceneID)[viewerLeaf]
	if raw == "" {
		return nil
	}
	return decodeViewerStateFromSnapshot(t, map[string]json.RawMessage{viewerLeaf: json.RawMessage(raw)})
}

func decodeViewerStateFromSnapshot(t *testing.T, state map[string]json.RawMessage) []ViewerRoom {
	t.Helper()
	raw, ok := state[viewerLeaf]
	if !ok {
		return nil
	}
	var js string
	if err := json.Unmarshal(raw, &js); err != nil {
		t.Fatalf("viewer leaf not a JSON string scalar: %s (%v)", raw, err)
	}
	var inj viewerInjection
	if err := json.Unmarshal([]byte(js), &inj); err != nil {
		t.Fatalf("viewer payload not decodable: %s (%v)", js, err)
	}
	return inj.Rooms
}

func waitForViewerToken(t *testing.T, w *Wire, sceneID, roomID, token string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, r := range decodeViewerState(t, w, sceneID) {
			if r.RoomID == roomID && r.JoinToken == token {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("viewer token for room %q never reached %q", roomID, token)
}
