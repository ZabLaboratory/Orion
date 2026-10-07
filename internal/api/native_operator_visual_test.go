package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/blueproject"
	"github.com/ZabLaboratory/Orion/internal/bluewire"
	"github.com/ZabLaboratory/Orion/internal/effects"
	"github.com/ZabLaboratory/Orion/internal/lsdpreception"
	"github.com/ZabLaboratory/Orion/internal/obs"
	"github.com/ZabLaboratory/Orion/internal/runtime"
	native "github.com/ZabLaboratory/Orion/third_party/lsdpnative"
)

// Driven by the offline CEF observer. Published program bytes stay unchanged;
// its actual read-only database effects use an authenticated test gateway.
func TestRealNativeOperatorVisual(t *testing.T) {
	phase := os.Getenv("ORION_TEST_OPERATOR_PHASE")
	if phase == "" {
		t.Skip("requires the native CEF operator proof")
	}
	if phase != "LEC" && phase != "LCK" {
		t.Fatal("unknown operator phase")
	}
	address, sourcePath, gateway := os.Getenv("ORION_TEST_LSDP_NATIVE_ADDRESS"), os.Getenv("ORION_TEST_LSML_PATH"), os.Getenv("ORION_TEST_QUERY_GATEWAY")
	if gateway == "" {
		t.Fatal("authenticated read gateway required")
	}
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	var original map[string]any
	if err = json.Unmarshal(source, &original); err != nil {
		t.Fatal(err)
	}
	id, version := original["scene_id"].(string), original["scene_version"].(string)
	key := lsdpreception.GenerationKey(id, version)
	program, err := os.ReadFile("testdata/scene-lec-lck.program.json")
	if err != nil {
		t.Fatal(err)
	}
	checksum := sha256.Sum256(program)
	if hex.EncodeToString(checksum[:]) != "9754a0b16acbaa5f061e9b45189b9a21443deb97ed37449d277b339f77a2b7b3" {
		t.Fatal("published Blue program bytes changed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	reception, err := lsdpreception.New(address, "orion/state")
	if err != nil {
		t.Fatal(err)
	}
	producer := lsdpreception.NewProducer(ctx, reception)
	defer producer.Close()
	hub := lsdpreception.NewHub(producer, testLogger())
	mirror := hub.MirrorForLSML(id, version, "on-air", source)
	digest := "sha256:ca2f21db4459f616c0f996fb7c03b3cf61e2a81d48bbc870d671f8fbda327bfc"
	instance := "cef-operator-" + phase
	host := bluehost.NewHost()
	handlers := bluehost.NewEffectHandlers(bluehost.EffectDeps{
		DB:          effects.NewDBQueryClient(gateway, "", &http.Client{Timeout: 12 * time.Second}),
		DataSources: map[string]effects.DataSource{"truth": {Name: "truth", Svc: "truth"}, "ranking": {Name: "ranking", Svc: "ranking"}},
	}, "execute")
	if err = host.Take(instance, id, digest, program, nil, nil, handlers); err != nil {
		t.Fatal(err)
	}
	defer host.Release(bluehost.SlotOnAir, "visual-test-end")
	bridges := bluewire.NewRegistry()
	bridge := bluewire.NewBridge(host, bluehost.SlotOnAir, mirror, id, digest, instance, blueproject.TargetProgram, "local-source-only", "cef-operator-native")
	bridge.SetLogger(testLogger())
	bridges.Start(bluehost.SlotOnAir, bridge, time.Hour, nil)
	defer bridges.StopAll()
	show := runtime.NewShow(runtime.NewComputeRegistry(), testLogger())
	defer show.Stop()
	mux := http.NewServeMux()
	RegisterPublic(mux, PublicDeps{Logger: testLogger(), Metrics: obs.NewMetrics(), Show: show, NativeLSDPFlush: producer.Flush, SceneIntent: &SceneIntentDeps{Host: host, Bridges: bridges}})
	server := httptest.NewServer(mux)
	defer server.Close()
	path := "/api/v1/operator/call/20d8ceaa-5766-4492-be94-8c58099519dc/" + phase
	request, _ := http.NewRequestWithContext(ctx, "POST", server.URL+path, bytes.NewBufferString(`{"payload":null}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Authenticated-Role", "operator")
	request.Header.Set("X-Authenticated-User", "native-visual-operator")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("operator %s: %d %s", phase, response.StatusCode, body)
	}
	client, err := native.Dial(ctx, address, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	raw, err := client.Exchange(ctx, map[string]any{"kind": "state.read", "target": "solar/generations"})
	if err != nil {
		t.Fatal(err)
	}
	var snapshot struct {
		State     map[string]map[string]any
		StateHash string
	}
	if err = json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	doc := snapshot.State[key]
	if doc == nil {
		t.Fatal("operator generation missing")
	}
	defaults := doc["defaults"].(map[string]any)
	names := map[string]any{}
	for _, side := range []string{"L", "R"} {
		for index := 0; index < 5; index++ {
			prefix := "pl." + side + string(rune('0'+index))
			name, _ := defaults[prefix+".name"].(string)
			champion, _ := defaults[prefix+".champ"].(string)
			if name == "" || name == "�" || champion == "" {
				t.Fatalf("actual match output missing at %s: name=%q champion=%q", prefix, name, champion)
			}
			names[prefix+".name"] = name
		}
	}
	if doc["x-orion"].(map[string]any)["runtime_instance_id"] != instance {
		t.Fatal("operator native provenance missing")
	}
	unchanged, err := os.ReadFile(sourcePath)
	if err != nil || !bytes.Equal(source, unchanged) {
		t.Fatal("original LSML was modified")
	}
	report, _ := json.Marshal(map[string]any{"entrypoint": phase, "path": path, "status": response.StatusCode, "nativeApplicationACK": true, "programDigest": digest, "programBytesSHA256": hex.EncodeToString(checksum[:]), "generation": key, "names": names, "originalLSMLUnchanged": true})
	t.Log("REAL_OPERATOR_NATIVE_PROOF " + string(report))
}
