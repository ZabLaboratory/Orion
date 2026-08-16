package lsdp

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	lproto "github.com/Lumencast/lumencast-go/protocol"

	"github.com/ZabLaboratory/Orion/internal/protocol"
)

// recordingSnapshotMetrics captures SnapshotIdentityGap calls for assertion.
type recordingSnapshotMetrics struct {
	gaps []string
}

func (r *recordingSnapshotMetrics) SnapshotIdentityGap(sceneID string) {
	r.gaps = append(r.gaps, sceneID)
}

// TestSceneMirror_SnapshotIdentityGap_KnownIdentityNoLongerCountedAsGap
// proves the post-lumencast-go-0c7cfc6 semantics: a Snapshot forward for a
// scene that DOES have a known projection identity (from a prior Delta) is
// still observed (Info log with the identity fields, for visibility), but is
// no longer counted as a gap. Before 0c7cfc6 this WAS a genuine,
// unavoidable loss (protocol.Snapshot, the kit's wire type, had no metadata
// field) and this test asserted a counted metric + Warn log. Since 0c7cfc6,
// the kit's Scene stamps its last known metadata (recorded on the exact same
// EmitWithCauseAndMetadata call that updates the mirror's own bookkeeping,
// against the exact same *lserver.Scene) onto every Snapshot it constructs
// — so the identity now reliably rides the frame instead of being dropped.
// Counting it as a gap would be a permanent false positive; see
// TestLSDP_SnapshotForwardCarriesKnownProjectionIdentity below for the
// direct, wire-level proof that the identity actually transits.
func TestSceneMirror_SnapshotIdentityGap_KnownIdentityNoLongerCountedAsGap(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	wire, err := NewWire(logger, nil)
	if err != nil {
		t.Fatalf("NewWire: %v", err)
	}
	metrics := &recordingSnapshotMetrics{}
	wire.SetSnapshotMetrics(metrics)
	mirror := wire.MirrorFor("scene-1", "sha256:test-1", nil).(*sceneMirror)

	mirror.Forward(&protocol.Delta{
		Patches:           []protocol.Patch{{Path: "score.team_a", Value: json.RawMessage(`14`)}},
		SchemaVersion:     "orion.blue-solar-projection.v1",
		SceneDigest:       "sha256:scene-1",
		RuntimeInstanceID: "runtime-1",
		Target:            "program",
		RenderRevision:    "render-7",
		CorrelationID:     "corr-7",
	})
	buf.Reset() // isolate the Snapshot-forward log from the Delta-forward one above

	mirror.Forward(&protocol.Snapshot{
		SceneVersion: "sha256:test-1",
		State:        map[string]json.RawMessage{"score.team_a": json.RawMessage(`14`)},
	})

	if len(metrics.gaps) != 0 {
		t.Fatalf("expected NO identity-gap counted (kit now carries the identity through), got %+v", metrics.gaps)
	}
	out := buf.String()
	for _, want := range []string{
		"lsdp snapshot reseed carries known projection identity",
		"scene_id=scene-1",
		"correlation_id=corr-7",
		"render_revision=render-7",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected identity-carried log to contain %q, got: %s", want, out)
		}
	}
	if strings.Contains(out, "drops known projection identity") {
		t.Fatalf("must not log a drop — the identity is no longer dropped: %s", out)
	}
}

// TestSceneMirror_SnapshotIdentityGap_NoGapWhenNothingKnownYet proves the
// honesty half of the fix: a Snapshot forwarded before any Delta ever
// carried a projection identity is NOT a gap — nothing was dropped — and
// must not be counted as one, only logged informationally. Unaffected by
// the lumencast-go bump: the kit still legitimately omits fields it never
// learned.
func TestSceneMirror_SnapshotIdentityGap_NoGapWhenNothingKnownYet(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	wire, err := NewWire(logger, nil)
	if err != nil {
		t.Fatalf("NewWire: %v", err)
	}
	metrics := &recordingSnapshotMetrics{}
	wire.SetSnapshotMetrics(metrics)
	mirror := wire.MirrorFor("scene-2", "sha256:test-2", nil).(*sceneMirror)

	mirror.Forward(&protocol.Snapshot{SceneVersion: "sha256:test-2", State: map[string]json.RawMessage{}})

	if len(metrics.gaps) != 0 {
		t.Fatalf("expected no identity-gap counted with nothing known yet, got %+v", metrics.gaps)
	}
	out := buf.String()
	if !strings.Contains(out, "no projection identity known yet") || !strings.Contains(out, "scene_id=scene-2") {
		t.Fatalf("expected informational log, got: %s", out)
	}
}

// TestSceneMirror_SnapshotIdentityGap_NilMetricsSinkIsNoOp proves
// SetSnapshotMetrics is genuinely optional (nil-safe, same posture as
// SetWSMetrics/SetExecMetrics elsewhere) — a wire that never had it called
// still logs (NewWire always installs a logger) but never panics or blocks
// on a nil metrics sink.
func TestSceneMirror_SnapshotIdentityGap_NilMetricsSinkIsNoOp(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	wire, err := NewWire(logger, nil)
	if err != nil {
		t.Fatalf("NewWire: %v", err)
	}
	mirror := wire.MirrorFor("scene-3", "sha256:test-3", nil).(*sceneMirror)

	mirror.Forward(&protocol.Delta{
		Patches: []protocol.Patch{{Path: "a", Value: json.RawMessage(`1`)}}, CorrelationID: "corr-3",
	})
	mirror.Forward(&protocol.Snapshot{SceneVersion: "sha256:test-3", State: map[string]json.RawMessage{"a": json.RawMessage(`1`)}})
	// No panic reaching here is the assertion; nil snapshotMetrics is a no-op.
}

// TestLSDP_SnapshotForwardCarriesKnownProjectionIdentity is the direct,
// wire-level proof (as opposed to the mirror-internal bookkeeping asserted
// above) that lumencast-go 0c7cfc6 closes the structural gap: a real LSDP
// subscriber, connected over a real WebSocket, receives a Snapshot frame
// whose six projection-metadata fields match the identity carried by the
// prior Delta — even though Orion's own Snapshot forward (mirror.go's
// *protocol.Snapshot case, driven by m.scene.Set) never itself constructs
// or attaches that metadata. It rides through purely via the kit's own
// Scene.lastMetadata / stampMetadata (server/scene.go), which
// EmitWithCauseAndMetadata populated on the preceding Delta forward.
func TestLSDP_SnapshotForwardCarriesKnownProjectionIdentity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wire, err := NewWire(quietLogger(t), nil)
	if err != nil {
		t.Fatalf("NewWire: %v", err)
	}
	mirror := wire.MirrorFor("scene-carry", "sha256:v1", nil).(*sceneMirror)

	wsURL := mountWire(t, wire)
	c := dialLSDP(ctx, t, wsURL, "operator", 0)
	defer c.Close(websocket.StatusNormalClosure, "")

	if _, ok := readServerFrame(ctx, t, c).(*lproto.Snapshot); !ok {
		t.Fatal("first frame must be the join snapshot")
	}

	mirror.Forward(&protocol.Delta{
		Patches:           []protocol.Patch{{Path: "score.team_a", Value: json.RawMessage(`14`)}},
		SchemaVersion:     "orion.blue-solar-projection.v1",
		SceneDigest:       "sha256:scene-carry",
		RuntimeInstanceID: "runtime-carry",
		Target:            "program",
		RenderRevision:    "render-42",
		CorrelationID:     "corr-42",
	})
	delta, ok := readServerFrame(ctx, t, c).(*lproto.Delta)
	if !ok {
		t.Fatal("second frame must be the Delta carrying the projection identity")
	}
	if delta.CorrelationID != "corr-42" {
		t.Fatalf("delta correlation_id = %q, want corr-42", delta.CorrelationID)
	}

	// A fresh Snapshot forward — the exact frame type that, before
	// lumencast-go 0c7cfc6, had no field to carry the identity at all.
	mirror.Forward(&protocol.Snapshot{
		SceneVersion: "sha256:v1",
		State:        map[string]json.RawMessage{"score.team_a": json.RawMessage(`14`)},
	})
	snap, ok := readServerFrame(ctx, t, c).(*lproto.Snapshot)
	if !ok {
		t.Fatal("third frame must be the reseeded Snapshot")
	}
	if snap.SchemaVersion != "orion.blue-solar-projection.v1" ||
		snap.SceneDigest != "sha256:scene-carry" ||
		snap.RuntimeInstanceID != "runtime-carry" ||
		snap.Target != "program" ||
		snap.RenderRevision != "render-42" ||
		snap.CorrelationID != "corr-42" {
		t.Fatalf("Snapshot frame did not carry the known projection identity: %+v", snap)
	}
}
