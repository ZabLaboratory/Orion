# Orion scene execution and selection

Canvas owns scene/Blue functional validation. This domain checks signed artifact
identity/integrity, exposes operator controls, and
coordinates the two Blue slots with the two physical Solar resources. Startup
wiring is in `cmd/orion/scene_control.go`; the feature map is in
`docs/development/scene-feature-map.md`.

`scene_intent.go` preserves principal/owner/tenant/stream/action checks, program
digests and per-lane admission leases. `scene_intent_source.go` verifies source,
program, optional compatibility-render bytes and optional Canvas Blue manifest.
No compiler runs at scene selection. Manifest transport integrity does not grant
execution authority; signed programs and refs are checked separately.

`scene_catalog.go` owns private immutable admitted capsules. Preparation through
`POST /api/v1/runtime/scene-catalog` does not run Blue or change the displayed
scene. Entries are keyed by principal, scene, version, stream and action; their
limit is 64 entries/64 MiB with the existing 1 MiB capsule limit. References from
the old external artifact cache are normalized to admitted bytes; that cache is
never written. A present Blue manifest is verified and retained, including its
declarations and closure. Older capsules without it keep signed executable
authority but cannot claim a complete authoring closure.
Manifest identity matches the source LSML address and signed revision_id, not
the artifact-set digest. Source-less compatibility envelopes retain their old
identity contract. `scene_intent_source_test.go` checks distinct identities and
refuses mismatched source/revision even with a correct manifest transport digest.

`scene_control.go` watches `/scene_control` in native `orion/state`. Only a
changed desired selection starts an activation; observed updates/reconnects do
not replay on-start. Each lane cancels and joins its predecessor before replacing
it. Only desired selection is saved to Orion's owned `selection.lsml`; live
mutated scene documents and runtime observations stay in RAM. On startup, absent
native control is seeded from that file, and every cached ref/program is
reverified. Expired refs remain a visible failure until newly admitted capsules
are supplied.

`scene_presentation.go` stages a fresh Blue instance without stepping it, pauses
the exact old bridge and freezes its scene projection. `native_scene_presentation.go`
prepares/commits/finalizes the selected Solar and waits for a renderer response
in `orion/state`. A native route receipt alone is insufficient. New on-start and
ticks are gated until acceptance. Cancellation/failure before finalization
restores the actual old host/bridge/mirror; compensation errors are explicit.
Global stream rules belong to `internal/streamcontrol` and remain independent.

Tests cover admission and identity, catalog isolation/corruption/expired refs,
desired-only persistence, staged replacement and compensation. Opt-in
`TestRealNativeSceneControlVisual` requires real Rust, two CEF Solar renderers,
the complete existing scene and authenticated read-query gateway; it also checks
every discovered command on both lanes, failed-source compensation, receiver
recovery and fresh Orion hosts. No business-output oracle runs in Orion.
See `docs/development/scene-control.md` for the wire contract.
Solar's `scripts/prove-cold-start-cef.py` additionally runs the actual Orion
executable across a full process stop/restart from owned caches, with two CEF
Solar renderers and the actual Rust receiver. Its test signatures and cache
manifest are explicit fixtures by default. Its `--capsule` mode instead consumes
the HTTP/SQL admission and signatures produced by an actual local Canvas using
the real gateway identity and published Blue. No admission verdict is seeded.
Remote Canvas deployment and mTLS workload retrieval remain separate boundaries.
See [current maturity](../../docs/development/maturity.md).

`operator_execution.go` reports the portable runtime dispatch for arbitrary calls
and resolved awaits, including global rules. The existing fired/resolved status
is preserved; the additive execution receipt carries runtime status and emitted
invocation IDs/capability/operation, without request or result payloads. These IDs
join the adapter completion diagnostics. It never declares the whole Blue finished while asynchronous
effects can still run. Public runtime failures retain code, stage, message and
target; scene replacement returns SCENE_CHANGING. No business outputs are sampled
in operator logs. Effect adapters log their completion and execution error.

`operator_execution_test.go` discovers all commands and armed awaits in arbitrary
transport fixtures on Program, Preview and stream rules, exercises each through
the registered HTTP router and checks runtime receipts/errors. This is a routing
regression test, not validation of authored Blue functionality. The real CEF
proof discovers the published scene's full command list. An unsupported test
payload widget fails coverage explicitly.

The historical `/validate/program` endpoint is retained because Canvas consumes
its execution diagnostics: isolated, bounded runtime load/start/step, without
external effects. Its `servable` field is the existing compatibility contract;
Canvas owns the scene validation decision. Scene activation never calls this
probe, simulates a Blue, or checks expected business outputs.
