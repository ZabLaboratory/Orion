package lsdp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	lproto "github.com/Lumencast/lumencast-go/protocol"
	"github.com/ZabLaboratory/Orion/internal/protocol"
	"github.com/coder/websocket"
)

func TestLSDP_ProjectionMetadataLiveAndReplay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wire, _, wsURL := dualShow(t)
	mirror := wire.MirrorFor("scene-1", "sha256:test-1", nil).(*sceneMirror)

	conn := dialLSDP(ctx, t, wsURL, "viewer", 0)
	defer conn.Close(websocket.StatusNormalClosure, "")
	if _, ok := readServerFrame(ctx, t, conn).(*lproto.Snapshot); !ok {
		t.Fatal("first frame: want snapshot")
	}

	mirror.Forward(&protocol.Delta{
		Patches:           []protocol.Patch{{Path: "score.team_a", Value: json.RawMessage(`14`)}},
		SchemaVersion:     "orion.blue-solar-projection.v1",
		SceneDigest:       "sha256:scene-1",
		RuntimeInstanceID: "runtime-1",
		Target:            "preview",
		RenderRevision:    "render-7",
		CorrelationID:     "corr-7",
	})

	live, ok := readServerFrame(ctx, t, conn).(*lproto.Delta)
	if !ok {
		t.Fatal("live frame: want delta")
	}
	assertProjectionMetadata(t, live, lproto.ProjectionMetadata{
		SchemaVersion:     "orion.blue-solar-projection.v1",
		SceneDigest:       "sha256:scene-1",
		RuntimeInstanceID: "runtime-1",
		Target:            "preview",
		RenderRevision:    "render-7",
		CorrelationID:     "corr-7",
	})
	resumeSeq := live.Seq
	_ = conn.Close(websocket.StatusNormalClosure, "")

	// Emit while the subscriber is away. The kit's replay buffer must retain
	// the metadata on the delta, not only its patches and sequence.
	mirror.Forward(&protocol.Delta{
		Patches:           []protocol.Patch{{Path: "score.team_a", Value: json.RawMessage(`21`)}},
		SchemaVersion:     "orion.blue-solar-projection.v1",
		SceneDigest:       "sha256:scene-1",
		RuntimeInstanceID: "runtime-1",
		Target:            "preview",
		RenderRevision:    "render-8",
		CorrelationID:     "corr-8",
	})

	resumed := dialLSDP(ctx, t, wsURL, "viewer", resumeSeq)
	defer resumed.Close(websocket.StatusNormalClosure, "")
	replay, ok := readServerFrame(ctx, t, resumed).(*lproto.Delta)
	if !ok {
		t.Fatal("replay frame: want delta")
	}
	if replay.Seq <= resumeSeq {
		t.Fatalf("replay seq %d is not after %d", replay.Seq, resumeSeq)
	}
	assertProjectionMetadata(t, replay, lproto.ProjectionMetadata{
		SchemaVersion:     "orion.blue-solar-projection.v1",
		SceneDigest:       "sha256:scene-1",
		RuntimeInstanceID: "runtime-1",
		Target:            "preview",
		RenderRevision:    "render-8",
		CorrelationID:     "corr-8",
	})
}

func TestMapProjectionMetadata_LegacyDeltaStaysAbsent(t *testing.T) {
	if got := mapProjectionMetadata(&protocol.Delta{}); got != nil {
		t.Fatalf("legacy delta metadata = %+v, want nil", got)
	}
}

func assertProjectionMetadata(t *testing.T, delta *lproto.Delta, want lproto.ProjectionMetadata) {
	t.Helper()
	got := lproto.ProjectionMetadata{
		SchemaVersion:     delta.SchemaVersion,
		SceneDigest:       delta.SceneDigest,
		RuntimeInstanceID: delta.RuntimeInstanceID,
		Target:            delta.Target,
		RenderRevision:    delta.RenderRevision,
		CorrelationID:     delta.CorrelationID,
	}
	if got != want {
		t.Fatalf("projection metadata = %+v, want %+v", got, want)
	}
}
