package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/ZabLaboratory/Orion/internal/attestation"
	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/bluewire"
	"github.com/ZabLaboratory/Orion/internal/effects"
	"github.com/ZabLaboratory/Orion/internal/lsdpreception"
	"github.com/ZabLaboratory/Orion/internal/runtime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Complete existing scene + unmodified published program + real native receiver
// + real CEF observer. Local test signatures are not production Canvas admission.
func TestRealNativeSceneControlVisual(t *testing.T) {
	if os.Getenv("ORION_TEST_SCENE_CONTROL_VISUAL") == "" {
		t.Skip("requires actual CEF scene-control observer")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	source, err := os.ReadFile(os.Getenv("ORION_TEST_LSML_PATH"))
	if err != nil {
		t.Fatal(err)
	}
	var original map[string]any
	if err := json.Unmarshal(source, &original); err != nil {
		t.Fatal(err)
	}
	id, version := original["scene_id"].(string), original["scene_version"].(string)
	program, err := os.ReadFile("testdata/scene-lec-lck.program.json")
	if err != nil {
		t.Fatal(err)
	}
	envelopeBytes, digest := canvasEnvelopeWithBundle(program, source)
	var envelope resolvedSceneEnvelope
	if err := json.Unmarshal(envelopeBytes, &envelope); err != nil {
		t.Fatal(err)
	}
	reception, err := lsdpreception.New(os.Getenv("ORION_TEST_LSDP_NATIVE_ADDRESS"), "orion/state")
	if err != nil {
		t.Fatal(err)
	}
	producer := lsdpreception.NewProducer(ctx, reception)
	defer producer.Close()
	hub := lsdpreception.NewHub(producer, testLogger())
	pub, priv, _ := ed25519.GenerateKey(nil)
	reSign := func(version string) string {
		token := signedRef(t, priv, "canvas-key-1", attestation.ActionTakeOnAir, time.Now(), id, digest)
		parts := strings.Split(token, ".")
		raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
		var claims map[string]any
		if err := json.Unmarshal(raw, &claims); err != nil {
			t.Fatal(err)
		}
		claims["artifact_set_digest"] = version
		claims["scene_digest"] = digest
		raw, _ = json.Marshal(claims)
		parts[1] = base64.RawURLEncoding.EncodeToString(raw)
		parts[2] = base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, []byte(parts[0]+"."+parts[1])))
		return strings.Join(parts, ".")
	}
	root := t.TempDir()
	catalog, err := OpenSceneCatalog(filepath.Join(root, "catalog"))
	if err != nil {
		t.Fatal(err)
	}
	deps := SceneIntentDeps{Trust: attestation.TrustSet{"canvas-key-1": pub}, LocatorPrefix: "scenes/", OwnerID: "owner-1", TenantID: "tenant-1", EmbeddedLocal: true, Host: bluehost.NewHost(), Catalog: catalog, Bridges: bluewire.NewRegistry(), WireFlush: producer.Flush, Logger: testLogger(), Idempotency: NewIdempotencyCache(),
		Effects: bluehost.EffectDeps{DB: effects.NewDBQueryClient(os.Getenv("ORION_TEST_QUERY_GATEWAY"), "", &http.Client{Timeout: 12 * time.Second}), DataSources: map[string]effects.DataSource{"truth": {Name: "truth", Svc: "truth"}, "ranking": {Name: "ranking", Svc: "ranking"}}}}
	laneFor := func(slot bluehost.Slot) *lsdpreception.Lane {
		if slot == bluehost.SlotOnAir {
			return hub.Lane("program")
		}
		return hub.Lane("preview")
	}
	deps.MirrorFor = func(id, version string, slot bluehost.Slot, source []byte) runtime.SceneMirror {
		return laneFor(slot).MirrorForLSML(id, version, source)
	}
	deps.Activate = func(id, _ string, slot bluehost.Slot) { laneFor(slot).SetActive(id) }
	deps.BeginLaneTransition = func(slot bluehost.Slot) func(bool) { return laneFor(slot).BeginTransition() }
	deps.PresentScene = NativeScenePresenter(reception, func(slot bluehost.Slot, id, version string, raw []byte) (map[string]any, error) {
		return laneFor(slot).PrepareSource(id, version, raw)
	})
	defer func() {
		deps.Bridges.StopAll()
		if err := deps.Host.Release(bluehost.SlotOnAir, "test"); err != nil {
			t.Fatal(err)
		}
		if err := deps.Host.Release(bluehost.SlotPreview, "test"); err != nil {
			t.Fatal(err)
		}
	}()
	headers := http.Header{"X-Authenticated-User": []string{"operator-1"}, "X-Authenticated-Role": []string{"operator"}}
	makeReq := func(action, version string) sceneIntentRequest {
		return sceneIntentRequest{IntentID: "prepare-catalog", StreamID: "stream-1", Action: action, ResolvedSceneRef: reSign(version), BlueProgram: envelope.BlueProgram, BlueProgramDigest: envelope.BlueProgramDigest, LSMLBundle: envelope.LSMLBundle, LSMLBundleDigest: envelope.LSMLBundleDigest}
	}
	for _, action := range []string{"prepare-preview", "take-on-air"} {
		raw, _ := json.Marshal(makeReq(action, version))
		request := httptest.NewRequest("POST", "/", bytes.NewReader(raw))
		request.Header = headers.Clone()
		response := httptest.NewRecorder()
		postSceneCatalog(deps)(response, request)
		if response.Code != 200 {
			t.Fatalf("catalog admission: %s", response.Body.String())
		}
	}
	path := filepath.Join(root, "selection.lsml")
	controller, err := OpenSceneControl(deps, reception, path, headers)
	if err != nil {
		t.Fatal(err)
	}
	controlCtx, stopControl := context.WithCancel(ctx)
	controlDone := make(chan error, 1)
	go func() { controlDone <- controller.Run(controlCtx) }()
	defer func() { stopControl(); <-controlDone }()
	wait := func(check func(map[string]any) bool) {
		t.Helper()
		until := time.Now().Add(20 * time.Second)
		for time.Now().Before(until) {
			state, err := reception.Read(ctx, "orion/state")
			if err == nil && check(state.(map[string]any)) {
				return
			}
			time.Sleep(40 * time.Millisecond)
		}
		t.Fatal("native scene-control condition timed out")
	}
	wait(func(state map[string]any) bool { return state["scene_control"] != nil })
	selected := func(lane string) bool {
		state, err := reception.Read(ctx, "orion/state")
		if err != nil {
			return false
		}
		root, _ := state.(map[string]any)
		control, _ := root["scene_control"].(map[string]any)
		defaults, _ := control["defaults"].(map[string]any)
		observed, _ := defaults["observed"].(map[string]any)
		status, _ := observed[lane].(map[string]any)
		return status["status"] == "active"
	}
	publishPhase := func(phase string) {
		raw, _ := json.Marshal(map[string]any{"type": "control-proof", "phase": phase})
		response, err := http.Post(os.Getenv("ORION_TEST_LOCAL_HTTP")+"/__render_diag", "application/json", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		time.Sleep(1200 * time.Millisecond)
	}
	selection := SceneSelection{SceneID: id, Version: version, StreamID: "stream-1"}
	if err = reception.WriteLeaf(ctx, "orion/state", "/scene_control/defaults/desired/program", selection); err != nil {
		t.Fatal(err)
	}
	wait(func(map[string]any) bool { return selected("program") })
	if !deps.Host.Serving(bluehost.SlotOnAir, id, digest) {
		t.Fatal("Program identity not admitted")
	}
	publishPhase("program-active")
	if err = reception.WriteLeaf(ctx, "orion/state", "/scene_control/defaults/desired/preview", selection); err != nil {
		t.Fatal(err)
	}
	wait(func(map[string]any) bool { return selected("preview") })
	publishPhase("preview-active")
	// Discover every authored command through the same contracts an operator
	// consumes. No command name or business-output oracle selects the coverage.
	var portable struct{ Entrypoints []struct{ Kind string } }
	if err = json.Unmarshal(program, &portable); err != nil {
		t.Fatal(err)
	}
	declaredCalls := 0
	for _, entry := range portable.Entrypoints {
		if entry.Kind == "call" {
			declaredCalls++
		}
	}
	var matchPayload any
	for _, lane := range []string{"program", "preview"} {
		selector := ""
		if lane == "preview" {
			selector = "?target=preview"
		}
		contractsRequest := httptest.NewRequest("GET", "/api/v1/cockpit/contracts?stream_id=stream-1"+func() string {
			if lane == "preview" {
				return "&target=preview"
			}
			return ""
		}(), nil)
		contractsRequest.Header = headers.Clone()
		contractsResponse := httptest.NewRecorder()
		public := PublicDeps{SceneIntent: &deps, NativeLSDPFlush: producer.Flush, Logger: testLogger()}
		getCockpitContracts(public)(contractsResponse, contractsRequest)
		var contracts cockpitContracts
		if contractsResponse.Code != 200 || json.Unmarshal(contractsResponse.Body.Bytes(), &contracts) != nil || len(contracts.Triggers) != declaredCalls || declaredCalls == 0 {
			t.Fatalf("command discovery incomplete: %s", contractsResponse.Body.String())
		}
		for index, command := range contracts.Triggers {
			var ui struct{ Widget string }
			if len(command.UI) > 0 && json.Unmarshal(command.UI, &ui) != nil {
				t.Fatal("invalid declared command UI")
			}
			var payload any
			switch ui.Widget {
			case "":
			case "match-selector":
				// Test-input acquisition only, using this published UI contract.
				// Orion never knows the meaning of the operator's opaque payload.
				if matchPayload == nil {
					rows, queryErr := deps.Effects.DB.Query(ctx, deps.Effects.DataSources["truth"], json.RawMessage(`{"table":"matches","select":["id"],"order":[{"column":"played_at","direction":"desc"}],"limit":1}`))
					if queryErr != nil {
						t.Fatal(queryErr)
					}
					var actual []map[string]any
					if json.Unmarshal(rows.Rows, &actual) != nil || len(actual) != 1 || actual[0]["id"] == nil {
						t.Fatal("match-selector input unavailable")
					}
					matchPayload = map[string]any{"match_id": actual[0]["id"]}
				}
				payload = matchPayload
			default:
				t.Fatalf("test payload not provided for widget %q; command not covered", ui.Widget)
			}
			body, _ := json.Marshal(map[string]any{"payload": payload})
			path := "/api/v1/operator/call/" + url.PathEscape(command.BlueprintKey) + "/" + url.PathEscape(command.EntrypointID) + selector
			request := httptest.NewRequest("POST", path, bytes.NewReader(body))
			request.Header = headers.Clone()
			request.SetPathValue("blueprint_id", command.BlueprintKey)
			request.SetPathValue("entrypoint_id", command.EntrypointID)
			response := httptest.NewRecorder()
			postOperatorCall(public)(response, request)
			if response.Code != 202 {
				t.Fatalf("operator %s/%s: %d %s", lane, command.EntrypointID, response.Code, response.Body.String())
			}
			requireDispatchReceipt(t, response, "fired")
			state, readErr := reception.Read(ctx, "solar/"+lane)
			if readErr != nil {
				t.Fatal(readErr)
			}
			doc, _ := state.(map[string]any)
			if doc["scene_id"] != id || doc["scene_version"] != version {
				t.Fatal("command delivered to a different scene")
			}
			phase := fmt.Sprintf("%s-command-%d", lane, index)
			var receipt any
			if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil {
				t.Fatal(err)
			}
			actual, _ := json.Marshal(map[string]any{"phase": phase, "lane": lane, "entrypoint": command.EntrypointID, "path": path, "receipt": receipt, "nativeStateRead": true, "projection": doc["x-orion"]})
			t.Log("REAL_OPERATOR_NATIVE_PROOF " + string(actual))
			publishPhase(phase)
		}
	}
	bridge := deps.Bridges.Current(bluehost.SlotOnAir)
	oldDigest := deps.Host.Digest(bluehost.SlotOnAir)
	bad := makeReq("take-on-air", "sha256:"+strings.Repeat("f", 64))
	bad.IntentID = "failed-stage"
	bad.IdempotencyKey = "failed-stage"
	raw, _ := json.Marshal(bad)
	request := httptest.NewRequest("POST", "/", bytes.NewReader(raw))
	request.Header = headers.Clone()
	response := httptest.NewRecorder()
	postSceneIntent(deps)(response, request)
	if response.Code == 200 || deps.Host.Digest(bluehost.SlotOnAir) != oldDigest || deps.Bridges.Current(bluehost.SlotOnAir) != bridge {
		t.Fatalf("failed source selection displaced active Blue: %s", response.Body.String())
	}
	publishPhase("rollback")
	// The crash observer kills only the owned Rust child after this marker.
	publishPhase("ready-for-crash")
	time.Sleep(3 * time.Second)
	if err = producer.Check(ctx); err != nil {
		t.Fatal(err)
	}
	publishPhase("after-crash")
	// Fresh controller + fresh Blue host reconstruct from the durable desired
	// config and reopened immutable catalog, after the native control is removed.
	stopControl()
	<-controlDone
	deps.Bridges.StopAll()
	if err := deps.Host.Release(bluehost.SlotOnAir, "restart"); err != nil {
		t.Fatal(err)
	}
	if err := deps.Host.Release(bluehost.SlotPreview, "restart"); err != nil {
		t.Fatal(err)
	}
	deps.Host = bluehost.NewHost()
	deps.Bridges = bluewire.NewRegistry()
	deps.Catalog, err = OpenSceneCatalog(filepath.Join(root, "catalog"))
	if err != nil {
		t.Fatal(err)
	}
	if err = reception.WriteLeaf(ctx, "orion/state", "/scene_control", nil); err != nil {
		t.Fatal(err)
	}
	controller, err = OpenSceneControl(deps, reception, path, headers)
	if err != nil {
		t.Fatal(err)
	}
	controlCtx, stopControl = context.WithCancel(ctx)
	defer stopControl()
	controlDone = make(chan error, 1)
	go func() { controlDone <- controller.Run(controlCtx) }()
	wait(func(map[string]any) bool { return selected("program") && selected("preview") })
	if !deps.Host.Serving(bluehost.SlotOnAir, id, digest) || !deps.Host.Serving(bluehost.SlotPreview, id, digest) {
		t.Fatal("fresh hosts not restored from owned cache")
	}
	publishPhase("orion-restart")
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted SceneControlDocument
	if json.Unmarshal(saved, &persisted) != nil || len(persisted.Defaults.Desired) != 2 || len(persisted.Defaults.Observed) != 0 {
		t.Fatal("durable selection contains runtime state")
	}
	if after, err := os.ReadFile(os.Getenv("ORION_TEST_LSML_PATH")); err != nil || !bytes.Equal(source, after) {
		t.Fatal("source LSML mutated")
	}
	t.Log("REAL_SCENE_CONTROL_PROOF PASS: signed catalog, desired LSML, two Solar lanes, every discovered command, exact old bridge after failed stage, receiver recovery, immutable source")
}
