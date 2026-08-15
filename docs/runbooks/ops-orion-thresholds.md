# Runbook — B3-R6-OPS-ORION operational thresholds

> ADR-BLUE-012 R6 §12/B8 — "B3-R6-OPS-ORION possède avant implémentation les
> fenêtres de replay/déduplication, limites d'instances et cache, seuils de
> backpressure, budgets providers et SLO." Orion#337. Absorbs Orion#274 (WS
> collapse/backpressure). Unblocks 18-PROBE (Blue#293).
> Owner: Keeper. No DB/SQLite/state_snapshot/durable credential introduced.

Each threshold below follows the ADR's required shape: unit, value,
justification, alert, nominal test, saturation test, fail-closed behavior,
rollback.

## 1. WS fanout backpressure / collapse (Orion#274)

| | |
|---|---|
| Unit / value | per-subscriber bounded channel, floor **16** messages (`Scene.Subscribe`, `internal/runtime/scene.go`) |
| Justification | pre-existing structural bound, unchanged by this unit — the gap was **observability**, not the bound itself |
| Alert | `orion_ws_dropped_total{scene_id,reason="collapse"}` rate > 0 sustained = a subscriber can't keep up with fanout; `reason="stuck_timeout"` rate > 0 = a subscriber is being force-closed |
| Nominal test | `internal/runtime/scene_test.go::TestScene_ConcurrentCloseDuringFanoutNeverPanics` |
| Saturation test | `internal/runtime/scene_test.go::TestScene_FanoutSaturationCollapsesToLatestSnapshotAndCounts` — 64 emits into an unread 16-slot queue; asserts the collapse is counted (`WSCollapsed`) and the consumer's final observed state is the LATEST scene state, never stale or empty |
| Fail-closed behavior | never a silent drop: a full queue is answered with a fresh-snapshot collapse (`Scene.collapseToSnapshot`), now counted (`orion_ws_dropped_total{reason="collapse"}`) and logged (`slog.Warn`). A subscriber still stuck after the 50ms drain-and-seed deadline is force-closed (`reason="stuck_timeout"`), counted, logged — the WS layer reconnects it |
| Rollback | revert `internal/runtime/scene.go` (`WSMetrics`, `collapseToSnapshot` wiring), `internal/runtime/show.go` (`SetWSMetrics`), `internal/obs/metrics.go` (`WSDropped` label change), `cmd/orion/main.go` (`show.SetWSMetrics`) — behavior degrades to the pre-existing (uncounted but still-collapsing) fanout, no data-path change |

**Root cause fixed**: `orion_ws_dropped_total` was declared and registered
(`internal/obs/metrics.go`) but never incremented anywhere — the collapse
path (`Scene.collapseToSnapshot`, already implemented for B8 ordering) had
zero observability. This unit wires the counter to that path; it does not
change the collapse mechanism itself.

## 2. Scene-intent replay/dedup window (§6.4)

| | |
|---|---|
| Unit / value | TTL **60s** (`ORION_IDEMPOTENCY_TTL_S`), cap **4096 entries** (`ORION_IDEMPOTENCY_MAX_ENTRIES`) — `internal/config/config.go` |
| Justification | 60s covers several retry cycles of a flaky Prism/Canvas caller without holding results indefinitely (§12 line 167: Gate B replay has "no crash-safe durability promised" — a long-lived durable dedup store would contradict that posture); 4096 entries bounds worst-case memory to a few MB of small typed responses |
| Alert | `orion_idempotency_evicted_total{reason="capacity"}` rate > 0 sustained = the window/cap pair is undersized for actual traffic — raise `ORION_IDEMPOTENCY_MAX_ENTRIES` |
| Nominal test | `internal/api/scene_intent_test.go::TestIdempotencyCache_TTLWindowExpiresAndIsCounted` |
| Saturation test | `internal/api/scene_intent_test.go::TestIdempotencyCache_SaturationBoundsMemoryAndIsCounted` — 64 distinct tuples into an 8-entry cache; asserts the map never exceeds the cap and eviction is counted |
| Fail-closed behavior | an evicted/expired tuple is a cache **miss**, never a denial — the scene-intent is reprocessed fresh (Prepare/Take re-runs). Bounding memory never blocks traffic; it only narrows the replay-suppression window |
| Rollback | revert `internal/api/scene_intent.go` (`IdempotencyCache` TTL+cap), `internal/config/config.go` (two env vars), `cmd/orion/main.go` (`NewIdempotencyCacheWithLimits` call) — falls back to the pre-existing unbounded map (functionally identical for replay suppression, loses the memory bound) |

**Root cause fixed**: `IdempotencyCache` (`internal/api/scene_intent.go`,
#331 stateless-cutover surface) was an unbounded `map[string]sceneIntentResponse`
with no TTL and no eviction — the "surcharge non bornée" risk §12 names
explicitly, and no replay *window* existed at all (an entry lived forever).

## 3. Engine-B instance limits (B7 isolation)

| | |
|---|---|
| Unit / value | exactly **2** concurrent instances per Host process: `SlotPreview` + `SlotOnAir` (`internal/bluehost/host.go`) |
| Justification | structural, not configurable — `Host.slots` is a `map[Slot]*entry` populated only by `Prepare(slot, ...)` and `Take(...)` (which always pins `SlotOnAir`); `Slot` has exactly two declared constants. There is no code path that creates a third concurrent instance |
| Alert | N/A — a third slot is a compile-time impossibility, not a runtime condition to watch |
| Nominal / saturation test | pre-existing `internal/bluehost` suite exercises Prepare/Take/Release across both slots; no code change in this unit — documented here per §12/B8's requirement that OPS-ORION *possess* (not necessarily re-implement) the threshold before 18-PROBE proceeds |
| Fail-closed behavior | `Take` on an existing on-air instance stops the previous one first (`h.slots[SlotOnAir]` reassignment) — no leak, no third instance |
| Rollback | N/A — no change made |

## 4. Cache (content-addressed bytes, ADR line 77)

No content-addressed byte cache exists in Orion today beyond the bounded
`IdempotencyCache` above (§2) and the per-request `BundledFetcher`
(`internal/compiler/bundled_fetcher.go`, request-scoped, not a growing
cache). `bluehost.Host.SetBundle`/`Bundle` store raw bytes per slot only —
bounded to the same 2 slots as §3, overwritten on each Prepare/Take, no
independent growth. **N/A**: no threshold to set until such a cache is
introduced; flagged here so a future PR adding one inherits this section
instead of skipping it.

## 5. Provider budgets (egress)

| | |
|---|---|
| Unit / value | **60 calls / 10s window, per stream** (`ORION_EGRESS_BUDGET_PER_STREAM` / `ORION_EGRESS_BUDGET_WINDOW_S`, `internal/effects/egress_budget.go`) |
| Justification | pre-existing (ADR Blue 009 Amendment 2 §B item 9, R3 condition of the G0 Bastion clearance) — reused verbatim per this unit's instruction, not reinvented |
| Alert | `orion_egress_budget_exceeded_total{scene_id}` rate > 0 sustained = a stream hitting (or being abused into) its egress cap |
| Nominal / saturation test | pre-existing `internal/effects` suite (token-bucket refill + exhaustion) |
| Fail-closed behavior | over-budget `core.service.call@1` fails closed to its `error` port (`EGRESS_BUDGET_EXCEEDED`) — never a crash, never a blocked tick |
| Rollback | N/A — no change made |

## 6. SLO / saturation summary

| Signal | Metric | Target |
|---|---|---|
| WS backpressure incidence | `rate(orion_ws_dropped_total{reason="collapse"}[5m])` | 0 under nominal load; transient spikes during a slow-client reconnect storm are expected and self-heal (collapse, not close) |
| WS force-close incidence | `rate(orion_ws_dropped_total{reason="stuck_timeout"}[5m])` | ~0 — a sustained rate means a client class cannot keep up even after a snapshot reset |
| Dedup window pressure | `rate(orion_idempotency_evicted_total{reason="capacity"}[5m])` | 0 — any sustained rate means the cap is undersized for the deployment |
| Egress budget pressure | `rate(orion_egress_budget_exceeded_total[5m])` | 0 outside intentional load tests |

No new dashboards are shipped in this unit — the four series above are
queryable immediately on the existing internal Prometheus scrape endpoint
once this PR deploys; wiring a Grafana panel is left to whoever owns the
dashboard repo (out of this dépôt's scope, per the ADR's mono-dépôt
boundary).
