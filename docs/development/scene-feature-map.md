# Scene intent feature map

All files remain in `internal/api` and the same Go package. HTTP registration, exported types, locks and state ownership are unchanged.

| Feature | File |
| --- | --- |
| Intent dependencies, request/response and admission orchestration | `scene_intent.go` |
| Bounded existing digest verification cache | `scene_intent_verification_cache.go` |
| Existing scoped intent replay and TTL/capacity limits | `scene_intent_idempotency.go` |
| Local artifact loading and envelope/digest verification | `scene_intent_artifacts.go` |
| Projection bridge lifecycle and slot release | `scene_intent_bridge.go` |
| Read-only slot/projection status | `scene_intent_status.go` |

No new cache, runtime reuse policy, endpoint, goroutine or timing setting is introduced. Existing `scene_intent*_test.go` tests continue exercising the same functions.

The compiler now separates `compile_blueprint.go`, `compile_platform_bindings.go` and
`compile_graph_helpers.go` from orchestration in `compile.go`. The runtime separates
input/evaluation methods in `scene_evaluation.go` and emission/backpressure methods in
`scene_emission.go`. All declarations stay in their original Go packages; the Scene
object, single-writer loop, channel ownership and method call sequence are unchanged.
