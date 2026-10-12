# Native scene production and shared reception

Prism owns the local native Rust server and passes ORION_LSDP_NATIVE_ADDRESS plus
ORION_LSDP_NATIVE_RESOURCE=orion/state. Orion performs a real native handshake and
resource read before serving. Its readiness endpoint rechecks that dependency and
returns 503 if unavailable. Orion never launches a duplicate receiver.

`producer.go` serializes publication off the scene/Blue goroutine. Full desired
documents use the receiver's state route: Rust computes its own Merkle diff.
Authored output leaves use atomic application transactions with the acknowledged
tree hash. A proven stale base is reread and the intended leaves rebased; unknown
transport outcomes replay the same transaction identity. Queue overflow or
unrecoverable delivery invalidates `/ready` instead of dropping an output.

Transport interruptions retain queued outputs and replay the same transaction
until the receiver responds or Orion shuts down. A verified success clears the
transient readiness error. An established socket failure gets one immediate
reconnect with the identical request; subsequent failures and initial dial
failure use the bounded 250 ms backoff. Idle expiry therefore does not add a
mandatory delay to the next edit. No scene/timer traffic is added to keep a socket
alive. `idle_recovery_test.go` checks immediate replay and outage backoff;
`idle_recovery_native_test.go` checks actual application after a private socket
loss. Failed dials never replace the live client: the native binding may return
a typed nil through its interface on cancellation; shutdown/retry must not call
Close on that value. The same regression suite covers cancelled real dials.
An uncertain commit followed by BASE_MISMATCH is
terminal: it is never rebased into a potentially duplicated insertion.
The native router retains each full assignment and its replay payload until its
owner releases the completed record. Both `Producer` and `Reception.Replace`
validate the exact completed route ID, then call `route.forget` with `force:false`.
After completion, a lost release receipt retries only the release; `unknown` is
an idempotent success. Replaying an already forgotten assignment could overwrite
a newer edit. Unconfirmed/rejected routes are never released. This bounds retained
router memory without increasing native quotas or removing another writer's records.
`/runtime/host-surface.native_delivery` exposes backlog/reconnect/delivery counts.
See `docs/runbooks/native-recovery.md` for process-restart limits.

Editable scene resources include `x-solar-authoring` (`solar.authoring/1`):
the immutable base, content-addressed non-font assets and font digests arrive by LSDP.
Font bytes remain in the installation/host font bank; scene changes do not resend them.
Only live root defaults change on edits; the base is preserved. Solar presentation
feedback also returns through the native `orion/state` resource. No Orion–Solar
HTTP source endpoint is registered.

`mirror.go` retains the original LSML, including geometry/styles/assets. It
projects runtime Blue output values only onto authored bindings, using the
existing render-surface gate and preserving structured JSON. Provenance travels
under `x-orion` in the same atomic mutation. Program and Preview are independent
resources. Immutable generation entries are keyed by SHA-256(scene ID + NUL +
admitted artifact-set digest), with bounded retention and writer leases so a delayed
Preview tick cannot overwrite a taken generation. Camera slots and viewer
credentials follow their lane and seed newly armed generations.
The native document preserves the source's LSML `scene_version` for Solar's
exact source acquisition. `x-orion-artifact-set` carries the separate admitted
generation identity; neither overwrites the other. Missing source identity is
an explicit delivery error. `producer_test.go` covers this boundary.

`session.go` publishes isolated test clones under their actual API session ID;
closing a session removes its entry and rejects late forwards. `orion/state`
carries rosters and overlay running/on-air control for Prism's native subscriber.
The executable no longer registers the old LSDP/1 rendering endpoints. The
legacy adapter package remains for explicit compatibility consumers and its
conformance tests; it is not instantiated by production startup.

Native test sessions start with a five-minute lease, renewed through the
operator-only URL returned by the open API and explicitly closed by its DELETE
URL. A periodic sweep reclaims abandoned clones and removes their native entry;
native subscriptions do not keep an Orion clone alive implicitly.

Scene-intent success waits for the native application ACK. Editable inputs first
cross the scene inbox and publish to their mirrors, then operator HTTP/WS
responses wait for the producer barrier. Camera projection uses the same ACK
boundary after the durable slot authority has accepted the change.
Operator calls and resolved awaits project their immediate runtime result through
the same bridge as ticks, then wait for native application. A later tick is not
required to publish an arbitrary command's immediate output.

The byte-pinned Go client is the unchanged optional Lumencast binding in
third_party/lsdpnative/client.go. Orion's `subscription.go` extends its existing
framing for control subscriptions; the pinned file remains unchanged. No native
server code is copied or modified.

`control.go` owns fragmented native control reads, CAS leaf assignments and
reconnecting subscriptions. Its `WriteLeavesFromState` builder recomputes only
state-dependent leaves after proven stale-base rejection, so Program retirement
preserves concurrent Preview desired selections. `program_identity.go` reads a
physical authority head; `program_release.go` pins its runtime/Take epoch, freezes
late mirror publications, reuses `Lane.Drop` and keeps a bounded 64-command replay
history. The native resource becomes null; the Lane identity and Preview document
stay intact. `program_release_test.go` checks empty-Program start, stale authority
refusal, retirement and late publication. Staged presentation uses idempotent full assignments
with the same identity across transport retries. `Lane.BeginTransition` freezes
Blue/camera projection until the coordinator restores or accepts the scene, then
publishes the latest camera authority. Control/scene lifecycle contracts belong
to [API admission](../api/README.md).

Checks: `go test ./...`, `go vet ./...`; `TestRealSharedNative` and
`TestRealNativeProducers` require `ORION_TEST_LSDP_NATIVE_ADDRESS` plus a real
`ORION_TEST_LSML_PATH`. The latter owns disposable native resources. The offline
CEF proof also runs a real validated Blue output program through `bluewire` and
the actual Rust receiver. Unit checks alone do not prove CEF or authenticated
Prism startup.
`TestRealNativeRouteRetention` requires `ORION_TEST_ROUTE_RETENTION_ADDRESS` for an
owned disposable node with a 128 KiB routing budget. It first proves `ROUTE_BYTES`
without release, then verifies 2,048 producer and 128 control replacements in the
same budget. It must never run against the shared development receiver.
`internal/api/TestRealNativeOperatorVisual` also invokes LEC and LCK using the
unaltered program published for the real scene, with actual read-only ZabTruth
and ZabRanking responses. The CEF report checks each roster, the native receipt,
submitted Vision frame, full-scene screenshots and retained physical camera.

## Persistent physical lanes

When the native receiver advertises both `solar/lane-a` and `solar/lane-b`,
startup enables physical routing. Logical Program/Preview Lane handles are stable;
their targets, active writers and camera/viewer authority permute only on a
validated `orion.lane-commit.v1` receipt. The Mirror and document remain on their
physical resource. Commit publishes no full LSML assignment; camera changes are
absolute leaf transactions. New Preview selection replaces only the outgoing
physical lane. Legacy resources remain available for older explicit consumers.

`lane_roles.go` owns correlation, revision/role validation, bounded exact replay,
conflict/stale/runtime/busy refusal and source identities. `lane_roles_test.go`
checks writer continuity and camera isolation. `api/lane_commit_native_test.go`
checks receipts and untouched documents with a disposable Rust receiver. Actual
Solar engine identity and rendered frames require Prism integration evidence.
