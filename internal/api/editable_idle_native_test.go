package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/canonical"
	"github.com/ZabLaboratory/Orion/internal/config"
	"github.com/ZabLaboratory/Orion/internal/lsdpreception"
	"github.com/ZabLaboratory/Orion/internal/runtime"
	"github.com/coder/websocket"
)

// Opt-in diagnostic on a disposable receiver. Time values are observations,
// never timing assertions: loaded Windows hosts need not meet a wall-clock SLA.
func TestEditableIdleDeltaNative(t *testing.T) {
	address := os.Getenv("ORION_TEST_PRIVATE_LANE_ADDRESS")
	if address == "" {
		t.Skip("requires disposable native receiver")
	}
	firstPause := 2 * time.Second
	if pause := os.Getenv("ORION_TEST_IDLE_PAUSE_MS"); pause != "" {
		ms, err := strconv.Atoi(pause)
		if err != nil || ms < 1 || ms > 180000 {
			t.Fatal("invalid idle probe duration")
		}
		firstPause = time.Duration(ms) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), firstPause+35*time.Second)
	defer cancel()
	reception, err := lsdpreception.New(address, "orion/state")
	if err != nil {
		t.Fatal(err)
	}
	producer := lsdpreception.NewProducer(ctx, reception)
	defer producer.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := lsdpreception.NewHub(producer, logger)
	hub.EnablePhysicalLanes()
	preview := runtime.NewPreviewSlot(ctx, runtime.NewComputeRegistry(), hub.Lane("preview"), logger)
	defer preview.Close()
	type nativeTiming struct {
		start, end    time.Time
		before, after lsdpreception.DeliveryStatus
	}
	timings := make(chan nativeTiming, 32)
	mux := http.NewServeMux()
	RegisterPublic(mux, PublicDeps{Logger: logger, Config: config.Config{Profile: config.ProfileEmbeddedLocal, LocalEditorToken: "idle-test-capability"}, Preview: preview, SceneIntent: &SceneIntentDeps{Host: bluehost.NewHost()}, NativeLSDPFlush: func(ctx context.Context) error {
		m := nativeTiming{start: time.Now(), before: producer.Status()}
		err := producer.Flush(ctx)
		m.end, m.after = time.Now(), producer.Status()
		timings <- m
		return err
	}})
	doc := map[string]any{"lsml": "1.2", "scene_id": "idle-native", "scene_version": "sha256:" + strings.Repeat("0", 64), "layout": map[string]any{"kind": "shape", "bind": map[string]any{"width": "__editable.node.width"}}, "defaults": map[string]any{"__editable.node.width": json.Number("10")}}
	version, err := canonical.Digest(doc)
	if err != nil {
		t.Fatal(err)
	}
	doc["scene_version"] = version
	path := "__editable.node.width"
	assets := map[string][]byte{}
	if fixture := os.Getenv("ORION_TEST_IDLE_SCENE_PATH"); fixture != "" {
		raw, err := os.ReadFile(fixture)
		if err != nil {
			t.Fatal(err)
		}
		var nativeDoc struct {
			Authoring struct {
				Bundle map[string]any    `json:"lsml_bundle"`
				Assets map[string][]byte `json:"assets"`
			} `json:"x-solar-authoring"`
		}
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.UseNumber()
		if decoder.Decode(&nativeDoc) != nil || nativeDoc.Authoring.Bundle == nil {
			t.Fatal("invalid native authoring fixture")
		}
		doc, assets = nativeDoc.Authoring.Bundle, nativeDoc.Authoring.Assets
		path = os.Getenv("ORION_TEST_IDLE_LEAF")
		if path == "" {
			t.Fatal("fixture needs ORION_TEST_IDLE_LEAF")
		}
	}
	opened := editableAPIRequest(t, mux, http.MethodPost, map[string]any{"scene_id": doc["scene_id"], "scene_version": doc["scene_version"], "render_mode": "source-only", "lsml_bundle": doc, "assets": assets}, "operator")
	if opened.Code != http.StatusOK {
		t.Fatal(opened.Code, opened.Body.String())
	}
	<-timings // activation's native barrier, not a leaf edit
	head, _ := preview.EditableTargetHead("preview")
	server := httptest.NewServer(mux)
	defer server.Close()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/api/v1/show/editable-preview.ws?local_editor_token=idle-test-capability", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	for i, pause := range []time.Duration{firstPause, 100 * time.Millisecond, 100 * time.Millisecond, 2 * time.Second, 100 * time.Millisecond, 100 * time.Millisecond, 6 * time.Second, 100 * time.Millisecond} {
		time.Sleep(pause)
		seq := uint64(i + 1)
		value := 0.148
		if i%2 != 0 {
			value = 0.74
		}
		raw, _ := json.Marshal(map[string]any{"target": "preview", "writer_id": head.WriterID, "scene_id": head.SceneID, "base_edit_seq": seq - 1, "edit_seq": seq, "patches": []map[string]any{{"path": path, "value": value}}})
		start := time.Now()
		if err = conn.Write(ctx, websocket.MessageText, raw); err != nil {
			t.Fatal(err)
		}
		_, reply, err := conn.Read(ctx)
		end := time.Now()
		if err != nil {
			t.Fatal(err)
		}
		var ack struct {
			Type    string
			EditSeq uint64 `json:"edit_seq"`
		}
		if json.Unmarshal(reply, &ack) != nil || ack.Type != "ack" || ack.EditSeq != seq {
			t.Fatal(string(reply))
		}
		m := <-timings
		state, err := reception.Read(ctx, "solar/lane-b")
		if err != nil {
			t.Fatal(err)
		}
		if state.(map[string]any)["defaults"].(map[string]any)[path] != value {
			t.Fatal("WS ACK did not follow actual native application")
		}
		proof, _ := json.Marshal(map[string]any{"seq": seq, "idle_ms": pause.Milliseconds(), "ws_ack_ms": float64(end.Sub(start).Microseconds()) / 1000, "ws_to_native_barrier_ms": float64(m.start.Sub(start).Microseconds()) / 1000, "native_barrier_ms": float64(m.end.Sub(m.start).Microseconds()) / 1000, "native_to_ws_ack_ms": float64(end.Sub(m.end).Microseconds()) / 1000, "before": m.before, "after": m.after, "native_value_verified": true})
		t.Log("IDLE_NATIVE_TIMING " + string(proof))
	}
}
