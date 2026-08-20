package main

import (
	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/compiler"
)

// bluehostRouteResolver adapts Blue's published curated-egress registry to
// the narrow host ABI. Blue remains the only producer of route metadata;
// Orion only re-keys the already fetched values for runtime lookup.
func bluehostRouteResolver(registry compiler.EgressRegistry) bluehost.ServiceRouteResolver {
	return func(service, routeID string) (bluehost.ServiceCallRoute, bool) {
		route, ok := registry[compiler.EgressRouteKey(service, routeID)]
		if !ok {
			return bluehost.ServiceCallRoute{}, false
		}
		return bluehost.ServiceCallRoute{
			Service:      route.Service,
			RouteID:      route.RouteID,
			Method:       route.Method,
			PathTemplate: route.PathTemplate,
			Params:       append([]string(nil), route.Params...),
			TokenPaths:   append([]string(nil), route.TokenPaths...),
		}, true
	}
}
