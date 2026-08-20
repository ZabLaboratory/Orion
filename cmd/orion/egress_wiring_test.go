package main

import (
	"testing"

	"github.com/ZabLaboratory/Orion/internal/compiler"
)

func TestBluehostRouteResolverUsesPublishedRoute(t *testing.T) {
	registry := compiler.EgressRegistry{
		compiler.EgressRouteKey("zabcam", "zabcam.slots.assign"): {
			Service:      "zabcam",
			RouteID:      "zabcam.slots.assign",
			Method:       "PUT",
			PathTemplate: "/cam/api/v1/cam/streams/{stream_id}/slots/{slot_ref}",
			Params:       []string{"stream_id", "slot_ref"},
			TokenPaths:   []string{"zabcam.slots.assign"},
		},
	}

	route, ok := bluehostRouteResolver(registry)("zabcam", "zabcam.slots.assign")
	if !ok {
		t.Fatal("published ZabCam route was not resolved")
	}
	if route.Method != "PUT" || route.PathTemplate != "/cam/api/v1/cam/streams/{stream_id}/slots/{slot_ref}" {
		t.Fatalf("resolved route = %+v", route)
	}
	if len(route.TokenPaths) != 1 || route.TokenPaths[0] != "zabcam.slots.assign" {
		t.Fatalf("resolved token paths = %#v", route.TokenPaths)
	}
}

func TestBluehostRouteResolverFailsClosedForUnknownRoute(t *testing.T) {
	if _, ok := bluehostRouteResolver(compiler.EgressRegistry{})("zabcam", "missing"); ok {
		t.Fatal("unknown route unexpectedly resolved")
	}
}
