package runtime

import (
	"sync"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/compiler"
)

// recordingMirror is a MirrorRegistry that records every EmitRoster call
// so the tests can assert the roster feed (IDs × versions) and the emit
// points (Load / re-push / Unload / SetActive).
type recordingMirror struct {
	mu      sync.Mutex
	rosters [][]RosterEntry
	active  string
}

func (m *recordingMirror) MirrorFor(_, _ string, _ *compiler.RenderBundle) SceneMirror {
	return noopMirror{}
}
func (m *recordingMirror) SetActive(id string) {
	m.mu.Lock()
	m.active = id
	m.mu.Unlock()
}
func (m *recordingMirror) Drop(string)                       {}
func (m *recordingMirror) EmitSlotAssignment(string, string) {}

func (m *recordingMirror) EmitOverlayApp(string, *bool, *bool) {}
func (m *recordingMirror) EmitRoster(entries []RosterEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rosters = append(m.rosters, append([]RosterEntry(nil), entries...))
}
func (m *recordingMirror) last() []RosterEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.rosters) == 0 {
		return nil
	}
	return m.rosters[len(m.rosters)-1]
}
func (m *recordingMirror) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rosters)
}

type noopMirror struct{}

func (noopMirror) Forward(SubscriberMsg) {}

func rosterGraph(id, version string) *compiler.Graph {
	return &compiler.Graph{SceneID: id, SceneVersion: version}
}

func rosterEqual(got []RosterEntry, want []RosterEntry) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestShow_RosterBuiltFromIDsAndVersions : the roster fed to the wire is
// the loaded scene set × each scene's graph.SceneVersion, sorted by id.
func TestShow_RosterBuiltFromIDsAndVersions(t *testing.T) {
	show := NewShow(NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)
	mirror := &recordingMirror{}
	show.SetMirrors(mirror)

	show.Load("beta", rosterGraph("beta", "sha256:bbb"), &compiler.RenderBundle{})
	show.Load("alpha", rosterGraph("alpha", "sha256:aaa"), &compiler.RenderBundle{})

	want := []RosterEntry{
		{SceneID: "alpha", SceneVersion: "sha256:aaa"},
		{SceneID: "beta", SceneVersion: "sha256:bbb"},
	}
	if got := mirror.last(); !rosterEqual(got, want) {
		t.Fatalf("roster mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

// TestShow_RosterEmittedAtEachMutationPoint : Load, re-push (version
// change), SetActive and Unload each publish a roster.
func TestShow_RosterEmittedAtEachMutationPoint(t *testing.T) {
	show := NewShow(NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)
	mirror := &recordingMirror{}
	show.SetMirrors(mirror)

	show.Load("s1", rosterGraph("s1", "sha256:v1"), &compiler.RenderBundle{})
	if mirror.count() != 1 {
		t.Fatalf("after first Load: want 1 emit, got %d", mirror.count())
	}

	// Re-push s1 with a new version — roster must reflect the new hash.
	show.Load("s1", rosterGraph("s1", "sha256:v2"), &compiler.RenderBundle{})
	if got, want := mirror.last(), []RosterEntry{{SceneID: "s1", SceneVersion: "sha256:v2"}}; !rosterEqual(got, want) {
		t.Fatalf("after re-push: roster %+v, want %+v", got, want)
	}

	before := mirror.count()
	if err := show.SetActive("s1", nil); err != nil {
		t.Fatal(err)
	}
	if mirror.count() <= before {
		t.Fatalf("SetActive did not emit a roster (count stayed %d)", before)
	}

	show.Unload("s1")
	if got := mirror.last(); len(got) != 0 {
		t.Fatalf("after Unload: roster should be empty, got %+v", got)
	}
}

// TestShow_NilMirrorRosterNoOp : with no wire (bespoke mode) the roster
// path is a no-op and never panics across Load / SetActive / Unload.
func TestShow_NilMirrorRosterNoOp(t *testing.T) {
	show := NewShow(NewComputeRegistry(), quietLogger())
	t.Cleanup(show.Stop)

	show.Load("s1", rosterGraph("s1", "sha256:v1"), &compiler.RenderBundle{})
	if err := show.SetActive("s1", nil); err != nil {
		t.Fatal(err)
	}
	show.Unload("s1")
	// Reaching here without a panic is the assertion.
}
