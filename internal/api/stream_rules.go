package api

import (
	"context"
	"log/slog"

	"github.com/ZabLaboratory/Orion/internal/compiler"
	"github.com/ZabLaboratory/Orion/internal/runtime"
	"github.com/ZabLaboratory/Orion/internal/store"
)

// Stream-level Blue rules (ADR 009 §3.1, issue #154) — the HTTP surface
// (POST/GET/DELETE /show/stream-rules) is RETIRED (#15, #331): porteur
// confirmed no bluehost multi-instance model is coming, so this feature
// does not survive the cutover — same documented-retirement posture as
// state_snapshot (ADR-BLUE-012 invariant #8), not a migration gap.
//
// ReloadBlueprintStreamRules is kept: it only REPLAYS rules persisted
// before this retirement (no HTTP path can create a new one anymore),
// preserving continuity for an install that still has them until the
// broader Store removal (#15's final phase) retires this too.

// ReloadBlueprintStreamRules reseeds the persisted blueprint-direct stream
// rules into the roster after a restart (#287). Distinct from reloadStreamRules
// (scene-based, which reseeds from stored pushed versions): a blueprint-direct
// rule has no carrier scene / pushed version, so it reseeds by re-fetching +
// recompiling from Blue (FetchBlueprint → CompileBlueprintRule(ctx, bp, bpID,
// fetcher) → ExecProgramsFromGraph, ADR 017). Only IDENTITY is durable: the
// rule reseeds from declared defaults and fires on-start once (criterion #11,
// ADR 009 §3.4) — no live leaf state is restored. Fail-soft per rule: an
// unreachable/deleted blueprint is skipped, never aborts boot. Must run AFTER
// the compiler fetcher is wired (unlike the scene reseed, which needs no
// fetcher), so cmd/orion calls it post-selectFetcher.
func ReloadBlueprintStreamRules(ctx context.Context, st store.Store, fetcher compiler.Fetcher, show *runtime.Show, logger *slog.Logger) {
	ids, err := st.ListBlueprintStreamRules(ctx)
	if err != nil {
		logger.Error("cold start: read blueprint stream rule set failed; rules stay dormant", "err", err)
		return
	}
	for _, id := range ids {
		bpID := id.String()
		bp, err := fetcher.FetchBlueprint(ctx, bpID)
		if err != nil {
			logger.Warn("cold start: blueprint stream rule fetch failed; skipped", "blueprint_id", bpID, "err", err)
			continue
		}
		graph, cerr := compiler.CompileBlueprintRule(ctx, bp, bpID, fetcher)
		if cerr != nil {
			logger.Warn("cold start: blueprint stream rule compile failed; skipped", "blueprint_id", bpID, "err", cerr)
			continue
		}
		progs, err := runtime.ExecProgramsFromGraph(graph)
		if err != nil {
			logger.Warn("cold start: blueprint stream rule exec decode failed; skipped", "blueprint_id", bpID, "err", err)
			continue
		}
		if err := show.PromoteBlueprintStreamRule(bpID, graph, &compiler.RenderBundle{}, progs...); err != nil {
			logger.Warn("cold start: blueprint stream rule promotion refused; skipped", "blueprint_id", bpID, "err", err)
		}
	}
}
