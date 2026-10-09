# Scene intent feature map

The shared native reception dependency is owned by `internal/lsdpreception`
([contract](../../internal/lsdpreception/README.md)), wired by `cmd/orion/main.go`
and checked by `/ready` in `public.go`. Prism starts the native process; this
producer publishes the scene intent and Blue projection to that same node.

HTTP admission belongs to `internal/api`; lane commit ownership belongs to the
shared Blue Host. Runtime and native publishers retain their existing owners.

| Feature | File |
| --- | --- |
| Desired scene LSML, lane supersession and owned restart selection | `scene_control.go`, `cmd/orion/scene_control.go`; [contract](scene-control.md) |
| Immutable admitted catalog and Canvas Blue closure retention | `scene_catalog.go`, `scene_blue_manifest.go`; `scene_catalog_test.go` |
| Renderer-confirmed staged replacement and exact-instance compensation | `scene_presentation.go`, `native_scene_presentation.go`, `internal/bluehost/replacement.go`; `scene_presentation_test.go` |
| Native control watch and bounded phase assignments | `internal/lsdpreception/control.go`, `third_party/lsdpnative/subscription.go` |
| Intent dependencies, request/response and admission orchestration | `scene_intent.go` |
| Shared per-lane cancellable admission commit lease | `internal/bluehost/scene_commit.go`; `scene_intent_concurrency_test.go` |
| Bounded existing digest verification cache | `scene_intent_verification_cache.go` |
| Existing scoped intent replay and TTL/capacity limits | `scene_intent_idempotency.go` |
| Local artifact loading and individual envelope/digest checks | `scene_intent_artifacts.go` |
| Separate program/source/optional compatibility-render verification | `scene_intent_source.go` |
| Optional signed compatibility-render handling; no LSML compilation on scene switch | `scene_intent_current_render.go` |
| Projection bridge lifecycle and slot release | `scene_intent_bridge.go` |
| Initial Blue on-start transition, then periodic clock ticks | `scene_intent_bridge.go`, `internal/bluewire/bridge.go`; `cockpit_source_test.go`, `bridge_test.go` |
| Native Preview/Program cockpit contracts and source-only operator interface | `cockpit.go`, `scenes_get.go`, `internal/bluehost/scene_interface.go`; `cockpit_source_test.go`, `scene_interface_test.go` |
| Global rule topic fan-out and reentrant event routing | `internal/bluehost/rule_plane.go`, `event_router.go`, `cmd/orion/main.go`; `rule_topics_test.go` |
| Generic operator execution receipts and public runtime errors | `operator_execution.go`, `operator.go`; `operator_execution_test.go` |
| Read-only slot/projection status | `scene_intent_status.go` |
| Ordered native full-state/leaf publication, retries and acknowledged hashes | `internal/lsdpreception/producer.go`, `tree.go` |
| Lost-receipt recovery without duplicated insertion; protocol/ambiguity refusal | `internal/lsdpreception/recovery_test.go`, `recovery_native_test.go`; native delivery diagnostics |
| Program/Preview resources, exact generation writer leases and camera/overlay projection | `internal/lsdpreception/mirror.go` |
| Isolated native test sessions, lease renewal, explicit close and expiry | `internal/lsdpreception/session.go`, `internal/runtime/test_session.go`, `show.go` |
| Native ACK on editable inputs and camera projection | `native_delivery.go`, `editable_preview.go`, `camera_slots.go`; runtime scene inbox barrier |
| Blue adapter discovery and configured availability | `host_surface.go`, `internal/providers/providers.go`; [Blue host](../../internal/bluehost/README.md) |
| Scene-local atomic structural edits and animation command leaves | `internal/lsdpreception/mutation.go`, `internal/bluehost/effect_animation.go`; `bluewire.Registry.Mutate` |
| Durable global rules, desired app intent and actual local process status | [stream control](../../internal/streamcontrol/README.md), `cmd/orion/stream_control.go`, `GET /api/v1/show/overlay-apps` |

Solar's source-only scene path preserves the source LSML address in native
`scene_version`, separately from the admitted generation's `artifact_set_digest`
carried by `x-orion-artifact-set`. It does not call the compiler or populate the
legacy render-bundle slot. Blue program identity remains `scene_digest`; Orion's
Blue host execution is gated during staged presentation and retains its previous
instance for compensation. Existing `scene_intent*_test.go`
tests exercise this boundary and the optional compatibility artifact.

The compiler now separates `compile_blueprint.go`, `compile_platform_bindings.go` and
`compile_graph_helpers.go` from orchestration in `compile.go`. The runtime separates
input/evaluation methods in `scene_evaluation.go` and emission/backpressure methods in
`scene_emission.go`. All declarations stay in their original Go packages; the Scene
object, single-writer loop, channel ownership and method call sequence are unchanged.

Source/render contracts and checks: [source-render boundary](source-render-boundary.md).

Source/Blue identity regression belongs to `scene_intent_identity_test.go`,
`scene_intent_source_test.go` and `internal/lsdpreception/producer_test.go`.
Retired compiler push-file scans are removed; current admission refusal coverage
is in scene_intent_test.go, scene_intent_source_test.go and scene_intent_noblue_test.go.

Current proof owner: [maturity](maturity.md). The full race battery covers the
producer/control/host surface; the four real-native opt-ins run separately.
Canvas actual local admission, full cold CEF restart and real Meet peer proofs
are referenced in the per-repository certificate, with deployment boundaries.
