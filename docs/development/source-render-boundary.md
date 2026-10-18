# Scene source and render boundary

Owner: `internal/api`; active scene-switch entry: `postSceneIntent`.

`Host.AcquireSceneCommit` gives each lane a commit lease shared by direct and
wrapper-created handlers. Identity, interface, mirror and the native delivery
barrier cannot interleave within that lane; the other lane progresses independently.
Concurrent replays recheck idempotency under the lease. Cancelled waiters do not
mutate Host. A failure after Host commit is not a transactional rollback.

`verifySceneArtifacts` validates the Blue program, LSML source and optional signed
compatibility render as separate artifacts. The LSML source is copied unchanged
to the LSDP mirror. The native document preserves the source's `scene_version`
for Solar's exact Canvas request. `x-orion-artifact-set` carries the separate
`artifact_set_digest` used by catalog/control/generation keys. Blue program
lookup and host identity continue to use `scene_digest`. The Blue manifest
matches the LSML address and signed revision_id. Solar's presentation ACK also
uses the LSML address; a real Canvas reference can have all three digests distinct.

## Solar source path

Solar resolves a scene identifier through ZabCanvas, downloads the exact
published LSMLZ (or LSML and its verified assets), then gives it to Vision.
Orion does not compile that LSML during scene intent. With no explicit render
capsule in the signed envelope, the host render-bundle slot remains empty and
the legacy render-bundle read returns no artifact. This is the normal path for
Solar's Vision renderer.

`prepareCurrentRender` only validates defaults from an explicitly attached,
signed compatibility capsule. It does not invoke `StaticBundleCompiler` or
create a render bundle from LSML. The compiler callback remains available to
separate validation and editable-preview integrations; it is not called by
scene switching.

Orion continues to verify and prepare Blue programs through its existing Blue
host lifecycle. Vision renders LSML and camera textures; it does not load or
execute Blue. ZabCanvas's validated Blue declaration manifest remains paired
with the pinned scene source for Solar's declaration and future local-cache
handling.

`Host.SetSceneInterface` attaches only the source's operator_inputs declaration
to the admitted slot. Cockpit Preview and Program contracts therefore remain
available without a render bundle. The exact-address operator-inputs route uses
scene_digest, preserving its existing addressing contract. Canonical LSML with
no inputs is authoritative and clears compatibility controls. Blue on-start is
executed through Bridge.StepOnce before periodic ticks; a tick cannot consume
that initial transition.

## Scene-selection control

The native Solar Program/Preview resources contain full LSML scene documents.
Replacing one with another full scene can switch Solar's rendering, but it
does not switch Orion's loaded Blue program, operator contracts or ingress.
The small `orion.scene-control.v1` LSML under `/scene_control` in `orion/state`
coordinates those responsibilities through desired/observed state. It names the
exact admitted catalog entry and lane. Orion subscribes, rechecks signed authority,
prepares the Blue/camera slot, stages the full native LSML and tracks Solar's
renderer ACK. Failure before finalization compensates the exact previous
host/bridge/frame; newer requests cancel and join stale preparations. Global
rules survive these switches. See [the wire contract](scene-control.md).

Orion's persisted stream-control.lsml is a startup snapshot written by the
controller. Editing that file externally is not a live subscribed command.
Prism still has its own persisted stream-rule selections and app reconciler;
this local change does not transfer those responsibilities or wire the installed
app manifest. Solar/native ACK also does not certify physical Program display.

## Validation

`scene_intent_source_test.go`, `scene_intent_identity_test.go`, `scene_intent_test.go` and
`scene_intent_noblue_test.go` prove that source-only switches pass the exact
source bytes to the mirror, use `artifact_set_digest` for admission generations, retain
`scene_digest` for Blue, leave the legacy render slot empty and make zero
compiler calls. Other scene-intent tests preserve optional signed compatibility
capsules and refusal behavior. Native `producer_test.go` checks that the LSML
address is preserved. `TestCanvasManifestUsesSourceVersionDistinctFromArtifactSet`
checks the source/manifest identity rather than substituting the artifact-set key.

`cockpit_source_test.go` adds source-only admission through both actual intent
actions, initial on-start awaits, input/trigger contracts and exact-address
reads. `scene_interface_test.go` proves declaration copy/empty/switch lifetime;
`rule_topics_test.go` proves global fan-out, rejection isolation and reentrant
rule emission/demotion without a plane lock.

`go test ./...` validates the complete local Go package suite. The change does
not alter Prism routes or deploy Orion. Physical Solar/CEF presentation is qualified separately in docs/development/maturity.md.
Solar's served-host startup synchronization and verified cache providers are
implemented. The installed Prism application's cache integration remains outside
this scope. Authenticated acquisition from deployed Canvas and local full-process
offline reconstruction have separate proof boundaries in maturity.md.
