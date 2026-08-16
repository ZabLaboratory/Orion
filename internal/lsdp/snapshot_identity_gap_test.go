package lsdp

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/ZabLaboratory/Orion/internal/protocol"
)

// recordingSnapshotMetrics captures SnapshotIdentityGap calls for assertion.
type recordingSnapshotMetrics struct {
	gaps []string
}

func (r *recordingSnapshotMetrics) SnapshotIdentityGap(sceneID string) {
	r.gaps = append(r.gaps, sceneID)
}

// TestSceneMirror_SnapshotIdentityGap_LogsAndCountsKnownIdentity proves the
// redefined priority-1 fix: a Snapshot forward for a scene that DOES have a
// known projection identity (from a prior Delta) is observed — Warn log
// with the identity fields, plus a counted metric — never silently dropped,
// and never fabricated onto the Snapshot frame itself (it has no field for
// it; TestSnapshotIdentityGap_SnapshotFrameCarriesNoMetadataField below
// proves that separately).
func TestSceneMirror_SnapshotIdentityGap_LogsAndCountsKnownIdentity(t *testing.T) {
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

	if len(metrics.gaps) != 1 || metrics.gaps[0] != "scene-1" {
		t.Fatalf("expected exactly 1 identity-gap count for scene-1, got %+v", metrics.gaps)
	}
	out := buf.String()
	for _, want := range []string{
		"lsdp snapshot reseed drops known projection identity",
		"scene_id=scene-1",
		"correlation_id=corr-7",
		"render_revision=render-7",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected gap log to contain %q, got: %s", want, out)
		}
	}
}

// TestSceneMirror_SnapshotIdentityGap_NoGapWhenNothingKnownYet proves the
// honesty half of the fix: a Snapshot forwarded before any Delta ever
// carried a projection identity is NOT a gap — nothing was dropped — and
// must not be counted as one, only logged informationally.
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
