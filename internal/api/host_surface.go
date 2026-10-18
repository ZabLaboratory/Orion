package api

import (
	"net/http"
	"sort"

	"github.com/ZabLaboratory/Orion/internal/providers"
)

// Host discovery supplements the portable descriptor with Orion's adapters and
// configured availability. A provider envelope alone is not a readiness claim.
func getHostSurface(deps PublicDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		availability := map[string]bool{}
		if d := deps.HostEffects; d != nil {
			availability["core.http.request"] = d.Egress != nil && len(deps.Config.HTTPEgressAllowHosts) > 0
			availability["core.db.query"] = d.DB != nil && len(d.DataSources) > 0
			availability["core.service.call"] = d.ServiceCall != nil && d.ResolveServiceRoute != nil
			availability["zabcam.slots"] = d.ServiceCall != nil && d.ResolveServiceRoute != nil && d.SlotMirror != nil
			availability["core.overlay-app"] = d.OverlayMirror != nil
		}
		if deps.SceneIntent != nil && deps.SceneIntent.Host != nil {
			availability["core.show.emit"], availability["core.lsml"], availability["core.animation"] = deps.SceneIntent.Host.SurfaceAvailability()
		}
		catalog := providers.Registry()
		for _, p := range catalog {
			if !availability[p["capability"].(string)] {
				p["health"] = "unavailable"
			}
		}
		effects := []map[string]any{
			{"opcode": "core.http.request@1", "capability": "core.http.request", "preview": "noop"},
			{"opcode": "core.http-request@1", "capability": "core.http.request", "preview": "noop"},
			{"opcode": "core.db.query@1", "capability": "core.db.query", "preview": "read-only"},
			{"opcode": "core.service.call@1", "capability": "core.service.call", "preview": "read-only"},
			{"opcode": "zabcam.assign-slot@1", "capability": "zabcam.slots", "preview": "blocked"},
			{"opcode": "core.show.emit@1", "capability": "core.show.emit", "preview": "noop"},
			{"opcode": "core.overlay-app.set@1", "capability": "core.overlay-app", "preview": "emulated"},
			{"opcode": "core.animation.play@1", "capability": "core.animation", "preview": "scene-local"},
		}
		for _, effect := range effects {
			effect["available"] = availability[effect["capability"].(string)]
		}
		sort.Slice(effects, func(i, j int) bool { return effects[i]["opcode"].(string) < effects[j]["opcode"].(string) })
		surface := map[string]any{
			"schema_version": "orion.blue-host-surface.v1", "providers": catalog, "effects": effects,
			"targets":     []string{"program", "preview", "stream-rule"},
			"entrypoints": []string{"start", "tick", "topic", "platform-event", "call"},
			"mutation":    map[string]any{"capability": "core.lsml", "operation": "mutate", "paths": []string{"/layout", "/defaults/*", "/animations"}, "operations": []string{"add", "remove", "replace", "test"}, "max_operations": 128, "persistence": "ram"},
		}
		if deps.NativeLSDPStatus != nil {
			surface["native_delivery"] = deps.NativeLSDPStatus()
		}
		writeJSON(w, http.StatusOK, surface)
	}
}
