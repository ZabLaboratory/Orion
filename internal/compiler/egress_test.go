package compiler

import (
	"encoding/json"
	"testing"
)

func echoRegistry() EgressRegistry {
	return EgressRegistry{
		EgressRouteKey("example", "example.echo"): {
			Service:      "example",
			RouteID:      "example.echo",
			Method:       "POST",
			PathTemplate: "/example/api/v1/items/{name}/echo",
			Params:       []string{"name"},
			TokenPaths:   []string{"example.echo"},
		},
	}
}

func serviceCallProg(service, routeID string) *execProgram {
	return &execProgram{
		Nodes: map[string]*execNode{
			"call": {ID: "call", Op: opServiceCall, Config: map[string]json.RawMessage{
				"service":  json.RawMessage(`"` + service + `"`),
				"route_id": json.RawMessage(`"` + routeID + `"`),
			}},
		},
	}
}

// TestResolveEgress_UndeclaredRejected: an undeclared (service, route_id)
// fails at compile with EGRESS_ROUTE_NOT_DECLARED (RC #1).
func TestResolveEgress_UndeclaredRejected(t *testing.T) {
	prog := serviceCallProg("example", "example.NOPE")
	diags := resolveEgressRoutes(prog, echoRegistry())
	if len(diags) != 1 || diags[0].Code != ErrEgressRouteNotDeclared {
		t.Fatalf("want one EGRESS_ROUTE_NOT_DECLARED, got %+v", diags)
	}
	if diags[0].Path != "call" {
		t.Errorf("diag path = %q, want call", diags[0].Path)
	}
}

// TestResolveEgress_NilRegistryFailsClosed: with no registry (fetcher
// without the optional method) every service.call rejects — fail-closed.
func TestResolveEgress_NilRegistryFailsClosed(t *testing.T) {
	prog := serviceCallProg("example", "example.echo")
	diags := resolveEgressRoutes(prog, nil)
	if len(diags) != 1 || diags[0].Code != ErrEgressRouteNotDeclared {
		t.Fatalf("want EGRESS_ROUTE_NOT_DECLARED on nil registry, got %+v", diags)
	}
}

// TestResolveEgress_DeclaredBakesRoute: a declared route bakes the curated
// method/path_template/token_paths into the node config under __route, and
// produces no diagnostic (RC #2/#3 — the runtime reads curated data).
func TestResolveEgress_DeclaredBakesRoute(t *testing.T) {
	prog := serviceCallProg("example", "example.echo")
	diags := resolveEgressRoutes(prog, echoRegistry())
	if len(diags) != 0 {
		t.Fatalf("declared route should not diagnose, got %+v", diags)
	}
	baked, ok := prog.Nodes["call"].Config[bakedRouteConfigKey]
	if !ok {
		t.Fatal("__route not baked into the node config")
	}
	var r bakedRoute
	if err := json.Unmarshal(baked, &r); err != nil {
		t.Fatal(err)
	}
	if r.Method != "POST" || r.PathTemplate != "/example/api/v1/items/{name}/echo" {
		t.Errorf("baked route = %+v", r)
	}
	if len(r.TokenPaths) != 1 || r.TokenPaths[0] != "example.echo" {
		t.Errorf("baked token_paths = %v", r.TokenPaths)
	}
}

// TestBuildEgressRegistry_ParsesManifestEnvelope: Blue's wire egress DTOs
// adapt into the keyed registry (RC #4 — shared source of truth).
func TestBuildEgressRegistry_ParsesManifestEnvelope(t *testing.T) {
	resp := blueManifestResponse{
		EgressRoutes: []blueEgressRoute{{
			Service:      "example",
			RouteID:      "example.echo",
			Method:       "POST",
			PathTemplate: "/example/api/v1/items/{name}/echo",
			Params:       []string{"name"},
			TokenPaths:   []string{"example.echo"},
		}},
	}
	reg := buildEgressRegistry(resp)
	route, ok := reg[EgressRouteKey("example", "example.echo")]
	if !ok {
		t.Fatal("route not in registry")
	}
	if route.Method != "POST" || len(route.TokenPaths) != 1 {
		t.Errorf("route = %+v", route)
	}
}
