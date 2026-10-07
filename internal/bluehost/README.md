# Blue host surface

Canvas owns functional validation of scene Blues. Orion executes the admitted
artifact and reports runtime/adapter errors; it has no business-output oracle.

`host.go` owns the two independent scene slots, Preview and Program. The global
`rule_plane.go` owns one portable instance per enabled stream rule. All three
use the same providers, route resolver, effect dependencies and runtime ABI.

`replacement.go` stages an unstepped candidate while retaining the exact old
instance. Calls, resolve, ticks and new platform events are blocked for that
slot during presentation. Rollback restores the old instance, including its
await/clock state; finalization releases it and opens the new execution gate.
The API coordinator owns the Solar/bridge/mirror transaction around this object.

`effects.go` handles dedicated HTTP, DB, service and camera opcodes.
`effect_http.go` admits generic invocations and completes them explicitly;
`effect_local.go` reuses the same DB/service/camera operations and adds local
show events, overlays, LSML mutation and animation commands. Unsupported or
unconfigured generic adapters complete with a failure instead of staying pending.
`providers.Registry` is the capability owner, not a second opcode catalogue.

`show_emit.go`, `effect_overlay.go` and `effect_animation.go` project the portable
runtime's cumulative side-effect bags once per node and causation identity.
Their history retains only the latest identity per node. `event_router.go`
queues nested scene/rule emissions with a 1024-event cascade bound. The same
router delivers to both active scene slots and every global rule. Rules that
do not declare a topic ignore it; invalid payloads and execution errors are
reported per rule without blocking other listeners. Fan-out snapshots targets
and releases the plane lock before runtime execution, including calls/ticks,
so nested emissions cannot deadlock a concurrent promotion/demotion.
Preview show emit and overlay set
do not write the Program control plane. Camera writes are forbidden in Preview.

`core.lsml/mutate` accepts up to 128 atomic add/remove/replace/test operations
on layout, individual defaults and animations. `bluewire.Registry.Mutate`
routes to the slot's current native mirror, including static scenes without a
Blue ticker. Source identity, Blue pins and camera authority cannot be rewritten.
`core.animation/play` carries `{animation_id, command_id}` to a stable default
leaf; Solar executes the declared asset. A new invocation replays the animation.

`GET /api/v1/runtime/host-surface` supplements the portable descriptor with
Orion adapters and configured availability. It does not certify that arbitrary
authoring extensions, undeclared apps or missing service routes exist.

Global rule persistence and local process ownership belong to
[`streamcontrol`](../streamcontrol/README.md). Tests cover effect parity,
entrypoints, Preview policy, reentrant events, native mutations and actual Marker
launch. The real native/Marker tests are opt-in and report skips explicitly.

`scene_interface.go` retains the operator declaration from admitted canonical
LSML, independently of an optional compatibility render bundle. Both cockpit
slots and exact-address `GET /scenes/{id}/operator-inputs?v={scene_digest}`
read this declaration; an empty source interface suppresses legacy controls.
The initial scene bridge executes one Step before periodic Ticks: Ticks alone
do not run Blue on-start entrypoints or arm initial operator awaits.

Limits: declaration discovery does not add a parameter-write API or a global
rule parameter interface. Generic invocation adapters do not themselves create
user-authorable Blue editor nodes. Native scene mutations cannot change scene
identity; changing the selection of a scene still needs Orion admission.
