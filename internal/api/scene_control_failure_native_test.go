package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ZabLaboratory/Orion/internal/lsdpreception"
)

// Uses only the disposable Rust node owned by the private test launcher.
func TestSceneControlFailedSelectionDoesNotLoopNative(t *testing.T) {
	address := os.Getenv("ORION_TEST_PRIVATE_LANE_ADDRESS")
	if address == "" {
		t.Skip("requires private native receiver")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	reception, err := lsdpreception.New(address, "orion/state")
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := OpenSceneCatalog(filepath.Join(t.TempDir(), "catalog"))
	if err != nil {
		t.Fatal(err)
	}
	deps := SceneIntentDeps{EmbeddedLocal: true, Catalog: catalog}
	control, err := OpenSceneControl(deps, reception, filepath.Join(t.TempDir(), "selection.lsml"), http.Header{})
	if err != nil {
		t.Fatal(err)
	}
	document := NewSceneControlDocument()
	document.Defaults.Desired["program"] = SceneSelection{"missing-catalog-scene", "version-1", "live"}
	if err := reception.WriteLeaf(ctx, "orion/state", "/scene_control", document); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = control.Run(ctx) }()
	defer func() { cancel(); <-done }()
	read := func() (SceneControlDocument, string) {
		state, err := reception.Read(ctx, "orion/state")
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(state.(map[string]any)["scene_control"])
		var doc SceneControlDocument
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		observed, _ := doc.Defaults.Observed["program"].(map[string]any)
		id, _ := observed["activation_id"].(string)
		return doc, id
	}
	awaitFailure := func(previous string) string {
		for ctx.Err() == nil {
			doc, id := read()
			observed, _ := doc.Defaults.Observed["program"].(map[string]any)
			if id != "" && id != previous && observed["status"] == "failed" && observed["reason"] == "SCENE_CATALOG_UNAVAILABLE" {
				return id
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("failure observation unavailable")
		return ""
	}
	first := awaitFailure("")
	for i := 0; i < 4; i++ {
		if err := reception.WriteLeaf(ctx, "orion/state", "/scene_control/defaults/observed/probe", i); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(250 * time.Millisecond)
	stable, id := read()
	if id != first {
		t.Fatal("observed updates retried failed selection", first, id)
	}
	stable.Defaults.Desired["program"] = SceneSelection{"missing-catalog-scene", "version-2", "live"}
	if err := reception.WriteLeaf(ctx, "orion/state", "/scene_control/defaults/desired", stable.Defaults.Desired); err != nil {
		t.Fatal(err)
	}
	second := awaitFailure(first)
	time.Sleep(250 * time.Millisecond)
	_, id = read()
	if id != second {
		t.Fatal("changed desired selection admitted repeatedly", second, id)
	}
	t.Log("NATIVE_FAILED_SELECTION_PROOF: one failed attempt per desired identity; four observed writes do not retry; changed version admits exactly one new attempt")
}
