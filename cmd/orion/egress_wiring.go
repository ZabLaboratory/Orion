package main

import (
	"context"
	"fmt"

	"github.com/ZabLaboratory/Orion/internal/bluehost"
	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/config"
)

// loadEgressRoutes selects the boot-time route source. The antenne profile
// keeps the historical Blue HTTP fetch. Embedded-local may instead consume
// the immutable bundle synchronized by Prism; after boot, route resolution is
// entirely local and the hot path never contacts Blue for metadata.
func loadEgressRoutes(ctx context.Context, cfg config.Config, serviceToken func([]string) string) (compiler.EgressRegistry, error) {
	if cfg.Profile.IsEmbeddedLocal() && cfg.SceneBundlePath != "" {
		bundle, err := compiler.LoadSceneBundle(cfg.SceneBundlePath)
		if err != nil {
			return nil, fmt.Errorf("load local scene bundle: %w", err)
		}
		routes, err := compiler.NewBundledFetcher(bundle).FetchEgressRoutes(ctx)
		if err != nil {
			return nil, fmt.Errorf("load local Blue egress routes: %w", err)
		}
		return routes, nil
	}

	fetcher := compiler.NewHTTPFetcherWithTokenFunc(cfg.CanvasBaseURL, cfg.BlueBaseURL, func() string {
		return serviceToken(cfg.ServicePaths)
	})
	routes, err := fetcher.FetchEgressRoutes(ctx)
	if err != nil {
		return nil, fmt.Errorf("load Blue curated egress routes: %w", err)
	}
	return routes, nil
}

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
