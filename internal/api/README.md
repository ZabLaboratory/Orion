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
changed desired selection starts an activation; failed admissions wait for a
changed desired identity instead of retrying on their own observations. Observed updates/reconnects do
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

Prepare carries the accepted LSML root and candidate source. Commit/finalize
carry the candidate once at the root with only request/phase metadata; a fresh
Solar can reconstruct that stage without duplicating the full source. Cleanup
still follows the renderer's matching active receipt before opening the Blue gate.
Each phase watches accepted native feedback before publishing, eliminating the
50 ms polling interval. Reception verifies/reconnects that bounded subscription;
the phase still has its 15 second deadline and reasserts the same absolute source
each second after interruption. Request, lane, phase and scene/version checks
remain mandatory, and cleanup cancels and joins the watcher.
Optional request-local timing callbacks distinguish Blue preparation, Solar
read/replace/wait/cleanup and bridge flush. HTTP `Server-Timing` contains numeric
durations only; no source, effect payload, credential or authorization changes.

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

Local editable Preview accepts `render_mode: source-only`: LSML identity and
canonical content address are verified before installing the isolated clone, with
no static tree lowering. The accepted document carries `x-solar-authoring` with
format `solar.authoring/1`, the immutable `lsml_bundle` base, embedded non-font assets and font digest references.
These travel atomically on the same native LSDP scene resource for Preview and
promoted generations. Live default edits remain LSDP mutations. The former HTTP
source GET is unregistered; Solar does not fetch editable sources from Orion.
This delivery does not grant Blue execution or publish a Canvas revision. Empty render mode
retains compatibility compilation; unknown modes fail closed.

Editable authoring accepts `target: preview|program` on POST, PUT and editor
WebSocket packets; omission preserves the Preview contract. The two actors stay
on the physical native resources transferred by Take. Program requires the
active `scene_id` and `writer_id`; a structural POST also requires
`base_edit_seq` and a non-decreasing `edit_seq`. Its replacement rotates the
writer ID. A Take preserves writer ID and sequence. Compiled snapshots are fenced
against concurrent edits and physical role changes before Host or mirror mutation.
`GET /api/v1/show/editable-preview/state?scene_id=...&target=...` returns only the
active editable identity, version, edit sequence, writer ID and live `defaults`,
with `no-store`. The read pins its lane and drains accepted actor inputs so values
and author sequence remain coherent; it publishes no mutation or snapshot.
It never activates, compiles or retrieves an inactive actor. Embedded/operator
guards remain mandatory. Service socket packets may explicitly bind the same
target/scene/writer; omitted fields retain the warm Preview component contract.
The compatibility `/air` generation actor is not a physical Program author;
only the native Take or replacement on the active Program establishes that role.

`editable_targets.go` owns target validation, active read-back and delivery;
`runtime/editable_targets.go` owns actor selection and structural fencing under
the role pin. Hot edits reuse the existing scene inbox, mirror and receiver
application ACK. Solar feedback and optional socket observers are not required.
Observers are scoped to their requested target. Tests in `editable_targets_test.go`
cover a socket retained across Take, stale writers and concurrent compilation.
`lane_commit_native_test.go` verifies active read-back, hot and structural Program
edits, unchanged Preview and stale snapshot refusal on a private Rust receiver.

## Native lane authority handoff

`lane_commit.go` consumes `/scene_control/defaults/lane_commit` on `orion/state`.
The parent is the boot-seeded `orion.scene-control.v1` document. Payload fields
are schema=`orion.lane-commit.v1`, runtime_instance_id, prepare_command_id,
take_command_id, intent_id, frame_id, pts_ns, server_seq, previous_role_map,
role_map, previous_revisions, revisions, scene_id and scene_version. Role maps
use `{on_air:A|B,preview:A|B}`; revisions use `{program,preview,role_map}`.
Both previous fields come directly from native TakeCommitted, after Prepare.
scene_version is the accepted source LSML version, distinct from artifact-set.

The response `/scene_control/defaults/observed/lane_commit` repeats the payload
with status=`committed|refused`, optional reason, physical targets and actual
sources `{scene_id,scene_version,artifact_set}`. Exact replay also reports
`replayed:true` without changing authority. Main checks exact correlation before
opening another preparation. Observe fields are applied atomically with logical
desired/current reconciliation from the actual mirrors, never a new scene intent.
Unknown runtime, stale/conflicting commit, busy preparation, mismatched Preview
or a refused Blue authority transfer cannot silently succeed.

Admission leases cover the two Host slots; bridge loops are cancelled/joined,
the existing Blue actors re-admitted in their new modes, and their same bridges
retargeted without Load/Start/Step/on-start. Editable Preview occupies a static
Host slot and supplies its native mirror to stream-level LSML effects, stopping
any older Preview Blue actor. Operator and platform-event execution/projection
share the slot lease so a result cannot cross a simultaneous role transfer.
Scene control serializes selection and role commit; a busy selection refuses the
commit. `NativeScenePresenter` fixes the physical target per admission and marks
all phase wrappers with their logical lane for Solar acknowledgments.

Unit tests live beside these handlers; the opt-in native test requires
ORION_TEST_PRIVATE_LANE_ADDRESS from an isolated synthetic receiver. It must
never target the shared Prism runtime. This path covers native signed Blue and
editable scenes; compatibility Engine A and standalone rule actors retain their
own execution contracts.
