//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/ZabLaboratory/Orion/internal/compiler"
)

// countingFetcher wraps a Fetcher and counts FetchCanvasLayout calls — the
// proxy for "a compile happened", since every Compile fetches the layout
// exactly once. Used to prove the switch-fix idempotence: an identical
// re-push must NOT re-compile (zero extra layout fetch).
type countingFetcher struct {
	compiler.Fetcher
	layoutCalls int32
}

func (f *countingFetcher) FetchCanvasLayout(ctx context.Context, v string) (*compiler.CanvasLayout, error) {
	atomic.AddInt32(&f.layoutCalls, 1)
	return f.Fetcher.FetchCanvasLayout(ctx, v)
}

// TestE2E_SwitchFix_IdenticalRePush_SkipsCompile proves task 1: a byte-
// identical re-push of the same scene skips Compile and every upstream fetch,
// resolving the already-persisted pushed version instead. The go-live flow
// pushes twice (push → validate → re-push); this kills the second compile's
// WAN cost.
func TestE2E_SwitchFix_IdenticalRePush_SkipsCompile(t *testing.T) {
	st := requireDB(t)
	sceneID := uuid.New()
	if _, err := st.CreateScene(context.Background(), sceneID, "switch-idem"); err != nil {
		t.Fatal(err)
	}
	cf := &countingFetcher{Fetcher: twoBlueprintFetcher()}
	srv, _ := gateTestServer(t, st, cf)
	base := srv.URL + "/api/v1/scenes/" + sceneID.String()
	body := `{"canvas_version":"v1","blue_blueprint_id":"bp-1"}`

	code1, b1 := operatorPost(t, base+"/push", body)
	if code1 != http.StatusOK {
		t.Fatalf("first push = %d %v", code1, b1)
	}
	if n := atomic.LoadInt32(&cf.layoutCalls); n != 1 {
		t.Fatalf("first push made %d layout fetches, want 1", n)
	}

	code2, b2 := operatorPost(t, base+"/push", body)
	if code2 != http.StatusOK {
		t.Fatalf("re-push = %d %v", code2, b2)
	}
	if n := atomic.LoadInt32(&cf.layoutCalls); n != 1 {
		t.Fatalf("identical re-push RE-COMPILED (layout fetches = %d, want 1) — idempotence broken", n)
	}
	if b2["idempotent"] != true {
		t.Fatalf("re-push not flagged idempotent: %v", b2)
	}
	if b1["scene_version"] != b2["scene_version"] {
		t.Fatalf("idempotent re-push minted a different version: %v vs %v", b1["scene_version"], b2["scene_version"])
	}
}

// TestE2E_SwitchFix_RePushActiveScene_NoReload proves task 3: an identical
// re-push of the LIVE scene is a silent no-op — no LoadExec, so the runtime
// keeps the SAME scene instance and the antenna never reloads. This closes
// the visible-reload / black-screen-recidive class (runbook 2026-06-29) where
// a redundant re-push took the antenna immediately.
func TestE2E_SwitchFix_RePushActiveScene_NoReload(t *testing.T) {
	st := requireDB(t)
	sceneID := uuid.New()
	if _, err := st.CreateScene(context.Background(), sceneID, "switch-guard"); err != nil {
		t.Fatal(err)
	}
	srv, show := gateTestServer(t, st, twoBlueprintFetcher())
	base := srv.URL + "/api/v1/scenes/" + sceneID.String()
	body := `{"canvas_version":"v1","blue_blueprint_id":"bp-1"}`

	if code, b := operatorPost(t, base+"/push", body); code != http.StatusOK {
		t.Fatalf("push = %d %v", code, b)
	}
	if code, _ := operatorPost(t, base+"/validate", `{}`); code != http.StatusAccepted {
		t.Fatalf("validate not accepted")
	}
	waitValidated(t, base)
	activateScene(t, srv.URL, sceneID)

	before := show.Active()
	if before == nil || before.ID() != sceneID.String() {
		t.Fatalf("scene not active before re-push")
	}

	code, resp := operatorPost(t, base+"/push", body)
	if code != http.StatusOK {
		t.Fatalf("identical re-push = %d %v", code, resp)
	}
	after := show.Active()
	if before != after {
		t.Fatalf("identical re-push of the LIVE scene reloaded the antenna: scene instance swapped (guard failed)")
	}
}
