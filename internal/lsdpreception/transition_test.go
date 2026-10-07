package lsdpreception

import (
	"context"
	"encoding/json"
	"github.com/ZabLaboratory/Orion/internal/protocol"
	"testing"
)

func TestLaneFreezesCameraAndBlueProjectionAndRestoresLatestAuthority(t *testing.T) {
	p := &Producer{ctx: context.Background(), queue: make(chan delivery, 8)}
	h := NewHub(p, nil)
	lane := h.Lane("program")
	source := []byte(`{"lsml":"1.2","scene_id":"scene-1","scene_version":"v1","layout":{"type":"frame"},"defaults":{}}`)
	mirror := lane.MirrorForLSML("scene-1", "v1", source)
	lane.SetActive("scene-1")
	<-p.queue
	for _, restore := range []bool{true, false} {
		finish := lane.BeginTransition()
		lane.EmitSlotAssignment("host", "new-camera")
		mirror.Forward(&protocol.Snapshot{State: map[string]json.RawMessage{"title": json.RawMessage(`"unsafe"`)}})
		if len(p.queue) != 0 {
			t.Fatal("projection escaped transition freeze")
		}
		finish(restore)
		job := <-p.queue
		var document map[string]any
		json.Unmarshal(job.document.(json.RawMessage), &document)
		if document["defaults"].(map[string]any)["__cam.slots.host"] != "new-camera" {
			t.Fatal("latest camera authority lost")
		}
	}
}
